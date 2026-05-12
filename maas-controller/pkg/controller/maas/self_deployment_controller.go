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

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// CleanupFinalizer was historically added to the maas-controller Deployment for coordinated
// teardown when ODH removed MaaS. It is no longer set; this constant remains so reconciles
// can strip it from older installs.
const CleanupFinalizer = "maas.opendatahub.io/cleanup"

// LifecycleReconciler watches the maas-controller Deployment and links it to Config/default
// via a non-controller ownerReference so the workload participates in the same GC graph as
// other operands. Legacy CleanupFinalizer entries are removed when present.
type LifecycleReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	DeploymentName   string
	DeploymentNS     string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=configs,verbs=get

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("self-deployment").WithValues("deployment", req.NamespacedName)

	var dep appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if dep.DeletionTimestamp.IsZero() {
		if err := r.ensureDeploymentReferencesConfig(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.stripLegacyCleanupFinalizer(ctx, log, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Terminating: remove legacy finalizer only so deletion is not blocked.
	if err := r.stripLegacyCleanupFinalizer(ctx, log, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LifecycleReconciler) stripLegacyCleanupFinalizer(ctx context.Context, log logr.Logger, key types.NamespacedName) error {
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(&dep, CleanupFinalizer) {
		return nil
	}
	base := dep.DeepCopy()
	controllerutil.RemoveFinalizer(&dep, CleanupFinalizer)
	if err := r.Patch(ctx, &dep, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("remove legacy cleanup finalizer from Deployment: %w", err)
	}
	log.Info("removed legacy cleanup finalizer from Deployment")
	return nil
}

// ensureDeploymentReferencesConfig links the controller Deployment to Config/default
// via a non-controller ownerReference so the workload participates in the same GC graph as other
// operands without competing with the ODH operator's controller owner (when present).
func (r *LifecycleReconciler) ensureDeploymentReferencesConfig(ctx context.Context, key types.NamespacedName) error {
	log := ctrl.LoggerFrom(ctx)
	if r.Scheme == nil {
		return nil
	}
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !cfg.DeletionTimestamp.IsZero() {
		return nil
	}
	if cfg.UID == "" {
		return nil
	}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	for _, ref := range dep.OwnerReferences {
		if ref.UID == cfg.UID && ref.Kind == maasv1alpha1.ConfigKind && ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return nil
		}
	}
	base := dep.DeepCopy()
	if err := controllerutil.SetOwnerReference(&cfg, &dep, r.Scheme); err != nil {
		return fmt.Errorf("set Config owner reference on deployment: %w", err)
	}
	if err := r.Patch(ctx, &dep, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch deployment ownerReferences: %w", err)
	}
	log.Info("set Config owner reference on maas-controller Deployment")
	return nil
}

// SetupWithManager registers the controller to watch only the maas-controller Deployment.
func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	selfOnly := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == r.DeploymentName && o.GetNamespace() == r.DeploymentNS
	})
	cfgSingleton := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == maasv1alpha1.ConfigInstanceName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(selfOnly)).
		Watches(
			&maasv1alpha1.Config{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(cfgSingleton),
		).
		Complete(r)
}
