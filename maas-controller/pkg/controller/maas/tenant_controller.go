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
	"sync"

	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// TenantReconciler reconciles the Tenant CRs that drive MaaS runtime manifests.
// Platform manifest logic follows the ODH operator's component deploy pattern (kustomize + post-render + SSA apply).
// The Tenant CR remains the runtime object while ModelsAsService carries the platform-facing aggregate status.
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace overrides POD_NAMESPACE / WATCH_NAMESPACE when discovering namespaced platform workloads (tests).
	OperatorNamespace string
	// ManifestPath is the directory containing kustomization.yaml for the ODH maas-api overlay (e.g. maas-api/deploy/overlays/odh).
	ManifestPath string
	// AppNamespace is the namespace where maas-api workloads are deployed (--maas-api-namespace,
	// default opendatahub for ODH, redhat-ods-applications for RHOAI).
	// Used by appNamespaceForTenant() and isProtectedNamespace().
	AppNamespace string
	// TenantNamespace is the namespace where the Tenant CR lives (--maas-subscription-namespace, default models-as-a-service).
	TenantNamespace string
	// GatewayName is the name of the Gateway resource resolved from cmd/manager flags.
	GatewayName string
	// GatewayNamespace is the namespace of the Gateway resource resolved from cmd/manager flags.
	GatewayNamespace string
	// cleanupOnce ensures legacy maas-api cleanup runs at most once per controller lifetime
	cleanupOnce sync.Once
	// cleanupMu protects cleanupCompleted
	cleanupMu sync.Mutex
	// cleanupCompleted tracks whether legacy cleanup succeeded
	cleanupCompleted bool
	// ClusterAudience is the OIDC audience resolved at startup (auto-detected issuer or default).
	ClusterAudience string
	// TenantNamespaceDiscoveryEnabled allows reconciling Tenant CRs across all namespaces
	// instead of only TenantNamespace (enables AITenant multi-tenancy).
	TenantNamespaceDiscoveryEnabled bool
	// MetadataCacheTTL is the TTL in seconds for Authorino metadata HTTP caching.
	// Applies to apiKeyValidation and subscription-info metadata evaluators.
	MetadataCacheTTL int64
}

// Tenant platform pipeline — resources the TenantReconciler creates and manages on behalf of maas-api.
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=configs,verbs=get;list;watch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.authorino.kuadrant.io,resources=authorinos,verbs=get;list;watch
// NOTE: TenantReconciler only watches (reads) these resources for status reporting and reconciliation logic.
// The actual Kuadrant resources are created and managed by specialized sub-controllers:
// - MaaSAuthPolicyReconciler creates authpolicies for per-tenant Authorino authentication setup
// - MaaSSubscriptionReconciler creates ratelimitpolicies/tokenratelimitpolicies for rate limiting
// - MaaSSubscriptionReconciler creates telemetrypolicies for observability and metrics collection
// TenantReconciler only needs read-only access (get;list;watch) to observe these resources for status.
// +kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuadrant.io,resources=ratelimitpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuadrant.io,resources=tokenratelimitpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuadrant.io,resources=telemetrypolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.kuadrant.io,resources=telemetrypolicies,verbs=get;list;watch
// Istio resources are created by other controllers or Istio itself; TenantReconciler only watches for status.
// +kubebuilder:rbac:groups=networking.istio.io,resources=destinationrules,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.istio.io,resources=envoyfilters,verbs=get;list;watch
// +kubebuilder:rbac:groups=telemetry.istio.io,resources=telemetries,verbs=get;list;watch
// Cronjobs, monitoring, and perses resources are created by other systems; TenantReconciler only watches.
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=podmonitors;servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=perses.dev,resources=persesdashboards;persesdatasources,verbs=get;list;watch

// clusterroles/clusterrolebindings: TenantReconciler reads RBAC state for validation only.
// Removed create/patch/delete verbs to eliminate RBAC privilege escalation vectors (CWE-269).
// Removed cluster-wide secrets access to eliminate privilege escalation surface (CWE-272).
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch

// Escalation-check mirror for maas-api ClusterRole — maas-controller must hold every verb it grants.
// namespaces create: bootstrap the subscription namespace at startup (ensureSubscriptionNamespaceWithClient).
// tokenreviews, subjectaccessreviews: required by maas-api for access checks.
// maasmodelrefs/maassubscriptions: read-only cross-reconciler references.
// Removed serviceaccounts/token create to eliminate privilege escalation primitive (CWE-269).
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=get;list;watch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions,verbs=get;list;watch

// Escalation-check mirror for payload-processing-reader ClusterRole — maas-controller must hold every verb it grants.
// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalmodels;externalproviders,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalmodels/status;externalproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalmodels/finalizers;externalproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=delete

// Reconcile drives the Tenant platform lifecycle. ODH deploys maas-controller; the controller
// owns the full deploy pipeline via the Tenant CR.
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

// crdLabeledForMaaSComponent matches CRDs labeled app.opendatahub.io/modelsasservice=true.
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
		return ns == r.AppNamespace || ns == r.TenantNamespace || ns == r.operatorNamespace()
	})
}

func authenticationClusterSingleton() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == openshiftAuthenticationClusterName
	})
}

func configResourceDefault() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == maasv1alpha1.ConfigInstanceName
	})
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
			&maasv1alpha1.Config{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.TenantNamespace,
					Name:      maasv1alpha1.TenantInstanceName,
				}}}
			}),
			builder.WithPredicates(configResourceDefault()),
		).
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
