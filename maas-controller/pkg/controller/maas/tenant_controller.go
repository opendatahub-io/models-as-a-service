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
	"strings"

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

// TenantReconciler reconciles cluster Tenant (platform singleton).
// Platform manifest logic mirrors opendatahub-operator modelsasservice (kustomize + post-render + SSA apply).
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace overrides POD_NAMESPACE / WATCH_NAMESPACE when discovering namespaced platform workloads (tests).
	OperatorNamespace string
	// ManifestPath is the directory containing kustomization.yaml for the ODH maas-api overlay (e.g. maas-api/deploy/overlays/odh).
	ManifestPath string
	// AppNamespace is the namespace where maas-api workloads are deployed (--maas-api-namespace, default opendatahub).
	AppNamespace string
	// TenantNamespace is the namespace where the Tenant CR lives (--maas-subscription-namespace, default models-as-a-service).
	TenantNamespace string
}

// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.authorino.kuadrant.io,resources=authorinos,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuadrant.io,resources=ratelimitpolicies,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=extensions.kuadrant.io,resources=telemetrypolicies,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=destinationrules,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=envoyfilters,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=telemetry.istio.io,resources=telemetries,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=perses.dev,resources=persesdashboards;persesdatasources,verbs=get;list;watch;create;patch;delete

// maas-controller creates the maas-api ClusterRole via SSA.
// The rules below mirror the maas-api ClusterRole so the controller can pass the API-server escalation check.
//
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=get;list;watch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions,verbs=get;list;watch

// Reconcile drives Tenant platform lifecycle (ODH no longer runs the modelsasservice deploy pipeline).
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcile(ctx, req)
}

const openshiftAuthenticationClusterName = "cluster"

func (r *TenantReconciler) enqueueDefaultTenant(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      maasv1alpha1.TenantInstanceName,
		Namespace: r.TenantNamespace,
	}}}
}

// crdLabeledForMaaSComponent matches ODH modelsasservice watch: app.opendatahub.io/modelsasservice=true.
func crdLabeledForMaaSComponent() predicate.Predicate {
	key := tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		l := o.GetLabels()
		return l != nil && l[key] == "true"
	})
}

// crdInOptionalAPIGroup matches CRDs belonging to optional platform operator API groups
// (e.g. perses.dev from COO). CRD names follow the pattern "<plural>.<group>", so a
// suffix check is sufficient to identify the group without parsing the spec.
func crdInOptionalAPIGroup() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		for group := range tenantreconcile.OptionalAPIGroups {
			if strings.HasSuffix(o.GetName(), "."+group) {
				return true
			}
		}
		return false
	})
}

func secretNamedMaaSDB() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == tenantreconcile.MaaSDBSecretName
	})
}

// inTenantWorkNamespaces limits watches to the namespaces where Tenant children live,
// avoiding cluster-wide informer noise on busy clusters.
func (r *TenantReconciler) inTenantWorkNamespaces() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		ns := o.GetNamespace()
		return ns == r.AppNamespace || ns == r.operatorNamespace()
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

// SetupWithManager registers the Tenant controller.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	authMeta := &metav1.PartialObjectMetadata{}
	authMeta.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "Authentication",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.Tenant{}).
		Watches(
			&extv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueDefaultTenant),
			builder.WithPredicates(crdLabeledForMaaSComponent()),
		).
		// Re-reconcile when optional operator CRDs (e.g. Perses from COO) are installed
		// so that resources previously skipped due to missing CRDs are applied immediately.
		Watches(
			&extv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueDefaultTenant),
			builder.WithPredicates(crdInOptionalAPIGroup()),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueDefaultTenant),
			builder.WithPredicates(deletedConfigMapOnly(), r.inTenantWorkNamespaces()),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueDefaultTenant),
			builder.WithPredicates(secretNamedMaaSDB(), r.inTenantWorkNamespaces()),
		).
		WatchesMetadata(
			authMeta,
			handler.EnqueueRequestsFromMapFunc(r.enqueueDefaultTenant),
			builder.WithPredicates(authenticationClusterSingleton()),
		).
		Complete(r)
}
