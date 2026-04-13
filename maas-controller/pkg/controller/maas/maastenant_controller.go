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

	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// MaaSTenantReconciler reconciles cluster MaaSTenant (platform singleton).
// Platform manifest logic mirrors opendatahub-operator modelsasservice (kustomize + post-render + SSA apply).
type MaaSTenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace overrides POD_NAMESPACE / WATCH_NAMESPACE when discovering namespaced platform workloads (tests).
	OperatorNamespace string
	// ManifestPath is the directory containing kustomization.yaml for the ODH maas-api overlay (e.g. maas-api/deploy/overlays/odh).
	ManifestPath string
	// AppNamespace is the fallback workloads namespace when DSCI cannot be read (typically the maas-api namespace / --maas-api-namespace).
	AppNamespace string
}

// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maastenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maastenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maastenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=dscinitialization.opendatahub.io,resources=dscinitializations,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Reconcile drives MaaSTenant platform lifecycle (ODH no longer runs the modelsasservice deploy pipeline).
func (r *MaaSTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcile(ctx, req)
}

const openshiftAuthenticationClusterName = "cluster"

func enqueueDefaultMaaSTenant(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaaSTenantInstanceName}}}
}

// crdLabeledForMaaSComponent matches ODH modelsasservice watch: app.opendatahub.io/modelsasservice=true.
func crdLabeledForMaaSComponent() predicate.Predicate {
	key := tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		l := o.GetLabels()
		return l != nil && l[key] == "true"
	})
}

func secretNamedMaaSDB() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == tenantreconcile.MaaSDBSecretName
	})
}

func authenticationClusterSingleton() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == openshiftAuthenticationClusterName
	})
}

// deletedConfigMapOnly mirrors ODH: unmanaged ConfigMaps are recreated when deleted.
func deletedConfigMapOnly() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(event.UpdateEvent) bool {
			return false
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

// SetupWithManager registers the MaaSTenant controller.
func (r *MaaSTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	authMeta := &metav1.PartialObjectMetadata{}
	authMeta.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "Authentication",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSTenant{}).
		Watches(
			&extv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(enqueueDefaultMaaSTenant),
			builder.WithPredicates(crdLabeledForMaaSComponent()),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(enqueueDefaultMaaSTenant),
			builder.WithPredicates(deletedConfigMapOnly()),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(enqueueDefaultMaaSTenant),
			builder.WithPredicates(secretNamedMaaSDB()),
		).
		WatchesMetadata(
			authMeta,
			handler.EnqueueRequestsFromMapFunc(enqueueDefaultMaaSTenant),
			builder.WithPredicates(authenticationClusterSingleton()),
		).
		Complete(r)
}
