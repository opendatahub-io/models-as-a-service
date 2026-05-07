/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// CleanupFinalizer is added to the maas-controller Deployment so that when ODH
// deletes it (on MaaS disable), the controller can clean up Tenant CRs, RBAC,
// and CRDs before the Deployment object is removed. Tenant finalizers cascade
// to delete maas-api, policies, and other children.
const CleanupFinalizer = "maas.opendatahub.io/cleanup"

// componentLabel selects all resources belonging to the MaaS component.
// The ODH operator stamps every deployed resource with app.opendatahub.io/<component-name>=true.
var componentLabel = client.MatchingLabels{"app.opendatahub.io/modelsasservice": "true"}

// requeueInterval is how often we recheck while Tenants are still terminating.
const requeueInterval = 5 * time.Second

// LifecycleReconciler watches the maas-controller Deployment and manages
// the cleanup finalizer lifecycle. While running it ensures CleanupFinalizer
// is present on the Deployment. When DeletionTimestamp is set (ODH disabled
// MaaS) it deletes all Tenant CRs, then removes cluster-scoped RBAC and CRDs,
// then releases the finalizer so the Deployment object can be fully removed.
// This gives Tenant's own finalizer time to clean up maas-api, auth policies,
// Perses dashboards, and other owned resources before the controller exits.
type LifecycleReconciler struct {
	client.Client
	DeploymentName  string
	DeploymentNS    string
	TenantNamespace string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=list;delete
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=list;delete

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("self-deployment").WithValues("deployment", req.NamespacedName)

	var dep appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if dep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.ensureFinalizer(ctx, log, &dep)
	}

	if !controllerutil.ContainsFinalizer(&dep, CleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	return r.runCleanup(ctx, log, &dep)
}

// ensureFinalizer adds the CleanupFinalizer to the Deployment when it is absent.
func (r *LifecycleReconciler) ensureFinalizer(ctx context.Context, log logr.Logger, dep *appsv1.Deployment) error {
	if controllerutil.ContainsFinalizer(dep, CleanupFinalizer) {
		return nil
	}
	controllerutil.AddFinalizer(dep, CleanupFinalizer)
	if err := r.Update(ctx, dep); err != nil {
		return fmt.Errorf("add cleanup finalizer to Deployment: %w", err)
	}
	log.Info("added cleanup finalizer to Deployment")
	return nil
}

// runCleanup drives the teardown sequence when the Deployment is terminating:
// delete Tenants, wait for them to be gone, then delete RBAC and CRDs, then
// release the finalizer.
func (r *LifecycleReconciler) runCleanup(ctx context.Context, log logr.Logger, dep *appsv1.Deployment) (ctrl.Result, error) {
	log.Info("Deployment is terminating; running cleanup before releasing finalizer")

	pending, err := r.deleteTenants(ctx, log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		log.Info("waiting for Tenant CRs to finish terminating")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if err := r.deleteClusterScopedResources(ctx); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(dep, CleanupFinalizer)
	if err := r.Update(ctx, dep); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove cleanup finalizer from Deployment: %w", err)
	}
	log.Info("removed cleanup finalizer from Deployment; cleanup complete")
	return ctrl.Result{}, nil
}

// deleteTenants deletes all Tenant CRs and returns true while any are still terminating.
func (r *LifecycleReconciler) deleteTenants(ctx context.Context, log logr.Logger) (pending bool, _ error) {
	tenantList := &maasv1alpha1.TenantList{}
	if err := r.List(ctx, tenantList, client.InNamespace(r.TenantNamespace)); err != nil {
		return false, fmt.Errorf("list Tenants in %q: %w", r.TenantNamespace, err)
	}
	for i := range tenantList.Items {
		t := &tenantList.Items[i]
		if !t.DeletionTimestamp.IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, t); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete Tenant %s/%s: %w", t.Namespace, t.Name, err)
		}
		log.Info("deleted Tenant", "namespace", t.Namespace, "name", t.Name)
		pending = true
	}
	return pending, nil
}

// deleteClusterScopedResources removes the cluster-scoped resources (ClusterRole,
// ClusterRoleBinding, CRDs) that the ODH operator deployed for maas-controller,
// selected by the component label.
//
// We list+delete individually rather than DeleteAllOf to avoid the deletecollection
// verb, which is often restricted in managed environments.
//
// Order matters: ClusterRoles and CRDs are deleted first while the ClusterRoleBinding
// still grants the SA the permissions to do so. ClusterRoleBindings are deleted last
// because removing them revokes all RBAC permissions for this SA.
func (r *LifecycleReconciler) deleteClusterScopedResources(ctx context.Context) error {
	crList := &rbacv1.ClusterRoleList{}
	if err := r.List(ctx, crList, componentLabel); err != nil {
		return fmt.Errorf("list maas-controller ClusterRoles: %w", err)
	}
	for i := range crList.Items {
		if err := r.Delete(ctx, &crList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ClusterRole %s: %w", crList.Items[i].Name, err)
		}
	}

	crdList := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := r.List(ctx, crdList, componentLabel); err != nil {
		return fmt.Errorf("list maas-controller CRDs: %w", err)
	}
	for i := range crdList.Items {
		if err := r.Delete(ctx, &crdList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete CRD %s: %w", crdList.Items[i].Name, err)
		}
	}

	// ClusterRoleBindings are deleted last: removing them revokes all cluster-scoped
	// permissions for the maas-controller SA, so this must come after ClusterRoles and CRDs.
	crbList := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(ctx, crbList, componentLabel); err != nil {
		return fmt.Errorf("list maas-controller ClusterRoleBindings: %w", err)
	}
	for i := range crbList.Items {
		if err := r.Delete(ctx, &crbList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", crbList.Items[i].Name, err)
		}
	}
	return nil
}

// SetupWithManager registers the controller to watch only the maas-controller Deployment.
func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	selfOnly := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == r.DeploymentName && o.GetNamespace() == r.DeploymentNS
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(selfOnly)).
		Complete(r)
}
