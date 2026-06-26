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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// MaaSAuthPolicyReconciler reconciles a MaaSAuthPolicy object
type MaaSAuthPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// MaaSAPINamespace is the namespace where maas-api service is deployed.
	// Used to construct the subscription selector endpoint URL.
	MaaSAPINamespace string

	// TenantNamespace is the namespace where the Tenant CR lives (configurable via flags).
	// Defaults to "models-as-a-service".
	TenantNamespace string

	// GatewayName is the name of the Gateway used for model HTTPRoutes (configurable via flags).
	GatewayName string
	// GatewayNamespace is the namespace of the Gateway used for model HTTPRoutes.
	GatewayNamespace string

	// TenantNamespaceDiscoveryEnabled enables AITenant-labeled tenant namespaces.
	TenantNamespaceDiscoveryEnabled bool

	// ClusterAudience is the OIDC audience of the cluster (configurable via flags).
	// Standard clusters use "https://kubernetes.default.svc"; HyperShift/ROSA use a custom OIDC provider URL.
	ClusterAudience string

	// MetadataCacheTTL is the TTL in seconds for Authorino metadata HTTP caching.
	// Applies to apiKeyValidation and subscription-info metadata evaluators.
	MetadataCacheTTL int64

	// AuthzCacheTTL is the TTL in seconds for Authorino OPA authorization caching.
	// Applies to auth-valid, subscription-valid, and require-group-membership authorization evaluators.
	AuthzCacheTTL int64

	// Recorder emits Kubernetes events for conflict detection warnings.
	Recorder record.EventRecorder
}

// oidcConfig holds OIDC configuration from Tenant CR
type oidcConfig struct {
	IssuerURL string
	ClientID  string
}

// authzCacheTTL returns the safe TTL for authorization caches that depend on metadata.
// Authorization cache entries must not outlive their dependent metadata cache entries,
// otherwise stale metadata can lead to incorrect authorization decisions.
// Returns the minimum of AuthzCacheTTL and MetadataCacheTTL, clamped to non-negative values.
func (r *MaaSAuthPolicyReconciler) authzCacheTTL() int64 {
	metadata := r.MetadataCacheTTL
	authz := r.AuthzCacheTTL

	// Defensive: clamp negative values to 0 (should be caught at startup, but defensive)
	if metadata < 0 {
		metadata = 0
	}
	if authz < 0 {
		authz = 0
	}

	if authz < metadata {
		return authz
	}
	return metadata
}

// fetchTenantIdentifier fetches the tenant identifier from the Tenant CR in the given namespace.
// The missing-Tenant fallback preserves legacy default-tenant behavior; malformed
// AITenant-managed Tenant metadata returns an error so callers do not collide with
// the legacy/default resource names.
func (r *MaaSAuthPolicyReconciler) fetchTenantIdentifier(ctx context.Context, log logr.Logger, policyNamespace string) (string, error) {
	tenant := &maasv1alpha1.Tenant{}
	tenantKey := client.ObjectKey{
		Name:      maasv1alpha1.TenantInstanceName,
		Namespace: policyNamespace,
	}

	if err := r.Get(ctx, tenantKey, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("Tenant not found, assuming default tenant (empty identifier)",
				"tenantName", maasv1alpha1.TenantInstanceName,
				"tenantNamespace", policyNamespace)
			// Fallback to default tenant identifier (empty string)
			return "", nil
		}
		log.Error(err, "failed to get Tenant resource",
			"tenantName", maasv1alpha1.TenantInstanceName,
			"tenantNamespace", policyNamespace)
		return "", err
	}

	// Use TenantIdentifierFor for resource naming (maas-api service name construction).
	// Returns "" for default tenant, tenantID for others.
	tenantIdentifier, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		log.Error(err, "failed to determine tenant identifier")
		return "", err
	}

	log.V(1).Info("Tenant identifier resolved", "tenantIdentifier", tenantIdentifier, "namespace", policyNamespace)
	return tenantIdentifier, nil
}

// fetchOIDCConfig fetches OIDC configuration from the Tenant CR in the given
// namespace. Each tenant namespace has its own Tenant/default-tenant with
// per-tenant OIDC settings. Returns nil if the Tenant CR doesn't exist or
// doesn't have externalOIDC configured.
func (r *MaaSAuthPolicyReconciler) fetchOIDCConfig(ctx context.Context, log logr.Logger, policyNamespace string) *oidcConfig {
	tenant := &unstructured.Unstructured{}
	tenant.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "maas.opendatahub.io",
		Version: "v1alpha1",
		Kind:    "Tenant",
	})

	tenantKey := client.ObjectKey{
		Name:      maasv1alpha1.TenantInstanceName,
		Namespace: policyNamespace,
	}

	if err := r.Get(ctx, tenantKey, tenant); err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			log.V(1).Info("Tenant CRD not installed or Tenant not found, OIDC support disabled",
				"tenantName", maasv1alpha1.TenantInstanceName,
				"tenantNamespace", policyNamespace)
			return nil
		}
		log.Error(err, "failed to get Tenant resource",
			"tenantName", maasv1alpha1.TenantInstanceName,
			"tenantNamespace", policyNamespace)
		return nil
	}

	// Extract spec.externalOIDC if present
	oidcSpec, found, err := unstructured.NestedMap(tenant.Object, "spec", "externalOIDC")
	if err != nil {
		log.Error(err, "failed to extract spec.externalOIDC from Tenant")
		return nil
	}
	if !found || oidcSpec == nil {
		log.V(1).Info("Tenant CR has no externalOIDC configuration")
		return nil
	}

	// Extract issuerUrl and clientId
	issuerURL, _, err := unstructured.NestedString(oidcSpec, "issuerUrl")
	if err != nil {
		log.Error(err, "Tenant externalOIDC.issuerUrl has invalid type (expected string)",
			"oidcSpec", oidcSpec)
		return nil
	}

	clientID, _, err := unstructured.NestedString(oidcSpec, "clientId")
	if err != nil {
		log.Error(err, "Tenant externalOIDC.clientId has invalid type (expected string)",
			"oidcSpec", oidcSpec)
		return nil
	}

	if issuerURL == "" {
		log.V(1).Info("Tenant externalOIDC has no issuerUrl")
		return nil
	}

	if clientID == "" {
		log.Error(nil, "Tenant externalOIDC has no clientId - audience validation is required for security")
		return nil
	}

	log.Info("OIDC configuration loaded from Tenant CR",
		"issuerUrl", issuerURL,
		"clientId", clientID)

	return &oidcConfig{
		IssuerURL: issuerURL,
		ClientID:  clientID,
	}
}

// fetchGatewayInfo fetches gateway namespace and name from the Tenant CR.
// Returns (gatewayNamespace, gatewayName, error).
// Falls back to controller defaults if Tenant CR not found or missing gateway ref.
func (r *MaaSAuthPolicyReconciler) fetchGatewayInfo(ctx context.Context, log logr.Logger, tenantNamespace string) (string, string, error) {
	tenant := &maasv1alpha1.Tenant{}
	tenantKey := client.ObjectKey{
		Name:      maasv1alpha1.TenantInstanceName,
		Namespace: tenantNamespace,
	}

	if err := r.Get(ctx, tenantKey, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			// No Tenant CR - use controller defaults
			log.V(1).Info("Tenant not found, using default gateway",
				"gatewayNamespace", r.GatewayNamespace,
				"gatewayName", r.GatewayName)
			return r.GatewayNamespace, r.GatewayName, nil
		}
		return "", "", fmt.Errorf("failed to get Tenant CR: %w", err)
	}

	// Check if Tenant has GatewayRef
	if tenant.Spec.GatewayRef.Namespace == "" || tenant.Spec.GatewayRef.Name == "" {
		// Tenant exists but no gateway ref - use controller defaults
		log.V(1).Info("Tenant has no gatewayRef, using defaults",
			"gatewayNamespace", r.GatewayNamespace,
			"gatewayName", r.GatewayName)
		return r.GatewayNamespace, r.GatewayName, nil
	}

	// Use tenant's gateway
	log.V(1).Info("Using tenant's gateway",
		"gatewayNamespace", tenant.Spec.GatewayRef.Namespace,
		"gatewayName", tenant.Spec.GatewayRef.Name)
	return tenant.Spec.GatewayRef.Namespace, tenant.Spec.GatewayRef.Name, nil
}

// CEL sub-expressions reused across Authorino cache-key selectors.
// These handle API keys, OIDC tokens, and Kubernetes tokens.
const (
	// celUserID extracts user ID from API key, OIDC, or K8s token
	// Used for cache keys (UUID for API keys, username for others)
	// API key: uses apiKeyValidation.userId (database UUID)
	// OIDC: uses preferred_username or sub (from JWT claims)
	// K8s: uses user.username (from TokenReview)
	celUserID = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.userId ` +
		`: (has(auth.identity.preferred_username) ? auth.identity.preferred_username ` +
		`: (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// celUsername extracts username for subscription ownership checks
	// Unlike celUserID (which uses UUID for API key cache keys), this always uses the actual username
	// API key: uses apiKeyValidation.username (service account name)
	// OIDC: uses preferred_username or sub (from JWT claims)
	// K8s: uses user.username (from TokenReview)
	celUsername = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.username ` +
		`: (has(auth.identity.preferred_username) ? auth.identity.preferred_username ` +
		`: (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// celGroups extracts groups from API key, OIDC, or K8s token
	// API key: uses apiKeyValidation.groups (snapshot at key creation)
	// OIDC: uses groups claim (no .user. prefix)
	// K8s: uses user.groups (from TokenReview)
	celGroups = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.groups ` +
		`: (has(auth.identity.groups) ? auth.identity.groups : auth.identity.user.groups)`

	celSubscription = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.subscription : ` +
		`("x-maas-subscription" in request.headers ? request.headers["x-maas-subscription"] : "")`
)

// celModelIdentity extracts model identity (namespace/name) from the request at gateway level.
// For path-routed inference (/<model-namespace>/<model-name>/...), extract from URL.
// For body-routed endpoints (/v1/*), use X-Gateway-Model-Name header (set by ext_proc).
// Canonical model IDs (publishers/{ns}/models/{name}) are normalized to {ns}/{name}.
// For listing endpoints like /v1/models where no model target exists, returns empty string
// so requestedModel is omitted and the subscription selector returns all accessible subscriptions.
const (
	celPathParts                  = `request.path.split("/").filter(x, x != "")`
	celPathModelIdentityAvailable = `size(` + celPathParts + `) >= 2 && ` +
		celPathParts + `[0] != "v1" && ` +
		celPathParts + `[0] != "maas-api"`
	celModelIdentityAvailable = `(` + celPathModelIdentityAvailable + ` || "x-gateway-model-name" in request.headers)`
	celModelIdentity          = `(` + celPathModelIdentityAvailable +
		` ? ` + celPathParts + `[0] + "/" + ` + celPathParts + `[1]` +
		` : ("x-gateway-model-name" in request.headers` +
		`   ? (request.headers["x-gateway-model-name"].startsWith("publishers/")` +
		`     ? request.headers["x-gateway-model-name"].split("/")[1] + "/" + request.headers["x-gateway-model-name"].split("/")[3]` +
		`     : request.headers["x-gateway-model-name"])` +
		`   : ""))`
)

// maasGatewayAuthPolicyName is the singleton AuthPolicy that targets the Gateway.
// All MaaSAuthPolicy CRs share this one policy; model identity is resolved dynamically.
const maasGatewayAuthPolicyName = "maas-gateway-auth"

// gatewayDefaultAuthPolicyName is the static deny-all AuthPolicy deployed by the Tenant
// reconciler. It must be deleted when maas-gateway-auth is created (two gateway-level
// AuthPolicies on the same target conflict in Kuadrant), and restored when the last
// MaaSAuthPolicy is removed so unconfigured models remain denied.
const gatewayDefaultAuthPolicyName = "gateway-default-auth"

// gatewayAuthzCacheKeySelector builds the cache-key expression for gateway-level
// model authorization checks: "userId|groups|modelIdentity".
// This prevents authz cache collisions across different model targets.
func gatewayAuthzCacheKeySelector() string {
	return fmt.Sprintf(
		`(%s) + "|" + (%s).join(",") + "|" + %s`,
		celUserID, celGroups, celModelIdentity,
	)
}

// subscriptionGatewayCacheKeySelector builds the cache-key expression for the gateway-level
// subscription-info and subscription-valid evaluators: "userId|groups|subscription|modelIdentity".
// Model identity is derived dynamically from X-Gateway-Model-Name header or request path.
func subscriptionGatewayCacheKeySelector() string {
	return fmt.Sprintf(
		`(%s) + "|" + (%s).join(",") + "|" + (%s) + "|" + %s`,
		celUserID, celGroups, celSubscription, celModelIdentity,
	)
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
//+kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
const maasAuthPolicyFinalizer = "maas.opendatahub.io/authpolicy-cleanup"

func (r *MaaSAuthPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSAuthPolicy", req.NamespacedName)

	policy := &maasv1alpha1.MaaSAuthPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSAuthPolicy")
		return ctrl.Result{}, err
	}

	// Handle deletion before tenant namespace gating. A namespace may lose its
	// discovery label while a CR is terminating; finalizer cleanup must still run.
	if !policy.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, policy)
	}

	isTenantNS, err := tenantNamespaceAllowed(ctx, r.Client, req.Namespace, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)
	if err != nil {
		log.Error(err, "failed to check tenant namespace")
		return ctrl.Result{}, err
	}
	if !isTenantNS {
		log.V(1).Info("ignoring MaaSAuthPolicy in non-tenant namespace", "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	// Handle no spec (e.g. legacy resources created before spec was required).
	// No finalizer needed — there are no AuthPolicies to clean up.
	if reflect.DeepEqual(policy.Spec, maasv1alpha1.MaaSAuthPolicySpec{}) {
		statusSnapshot := policy.Status.DeepCopy()
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseInvalid, "spec is required", statusSnapshot)
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		controllerutil.AddFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	statusSnapshot := policy.Status.DeepCopy()

	// Track missing models to include in status even when reconciliation skips them
	missingModels := r.findMissingModelRefs(ctx, policy)

	modelAllowlists, err := r.aggregateModelSubjectAllowlists(ctx, policy.Namespace)
	if err != nil {
		log.Error(err, "failed to aggregate per-model subjects for gateway AuthPolicy")
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to aggregate per-model access rules: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}
	modelAllowlistsJSON, err := json.Marshal(modelAllowlists)
	if err != nil {
		log.Error(err, "failed to marshal per-model subject aggregation")
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to serialize per-model access rules: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	oidc := r.fetchOIDCConfig(ctx, log, req.Namespace)
	tenantID, err := r.fetchTenantIdentifier(ctx, log, req.Namespace)
	if err != nil {
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to resolve tenant identifier: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	gatewayNs, gatewayName, err := r.fetchGatewayInfo(ctx, log, req.Namespace)
	if err != nil {
		log.Error(err, "failed to fetch gateway info")
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to fetch gateway info: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	// Reconcile the gateway-level AuthPolicy for this tenant's gateway.
	// In single-tenant mode: creates AuthPolicy for the default gateway.
	// In multi-tenant mode: creates AuthPolicy for each tenant's gateway.
	//
	// Skip reconciling if this is a non-default tenant using the default gateway.
	// This happens when a Tenant CR exists without a gatewayRef - it shouldn't
	// overwrite the default gateway's AuthPolicy with tenant-specific configuration.
	isDefaultGateway := gatewayNs == r.GatewayNamespace && gatewayName == r.GatewayName
	isNonDefaultTenant := tenantID != ""
	if isNonDefaultTenant && isDefaultGateway {
		log.Info("skipping gateway AuthPolicy reconciliation: non-default tenant falling back to default gateway (Tenant CR missing gatewayRef)",
			"tenantID", tenantID,
			"tenantNamespace", req.Namespace,
			"gatewayNamespace", gatewayNs,
			"gatewayName", gatewayName)
		// Still mark the policy as Active since the model-level auth rules are aggregated correctly,
		// even though we're not updating the gateway policy
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseActive, "", statusSnapshot)
		return ctrl.Result{}, nil
	}

	if err := r.reconcileGatewayAuthPolicy(ctx, log, string(modelAllowlistsJSON), oidc, tenantID, gatewayNs, gatewayName); err != nil {
		log.Error(err, "failed to reconcile gateway AuthPolicy")
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to reconcile gateway AuthPolicy: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	refs, err := r.reconcileModelAuthPolicies(ctx, log, policy)

	if err != nil {
		log.Error(err, "failed to reconcile model group AuthPolicies")
		r.updateStatus(ctx, policy, maasv1alpha1.PhaseFailed, fmt.Sprintf("Failed to reconcile: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	// Update per-AuthPolicy status
	r.updateAuthPolicyRefStatus(ctx, log, policy, refs)

	// Detect conflicting (non-MaaS) AuthPolicies on MaaS-managed HTTPRoutes
	prevConflict := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	conflicts, detectErr := r.detectConflictingAuthPolicies(ctx, log, policy)
	if detectErr != nil {
		apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               ConditionConflictingAuthPolicy,
			Status:             metav1.ConditionUnknown,
			Reason:             "ConflictCheckFailed",
			Message:            detectErr.Error(),
			ObservedGeneration: policy.GetGeneration(),
		})
	} else {
		setConflictingAuthPolicyCondition(policy, conflicts)
	}
	currConflict := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	shouldEmitConflictEvent := currConflict != nil &&
		currConflict.Status == metav1.ConditionTrue &&
		(prevConflict == nil ||
			prevConflict.Status != currConflict.Status ||
			prevConflict.Message != currConflict.Message)
	if shouldEmitConflictEvent && r.Recorder != nil {
		var names []string
		for _, c := range conflicts {
			names = append(names, c.String())
		}
		r.Recorder.Eventf(policy, "Warning", "ConflictingAuthPolicy",
			"Detected %d non-MaaS AuthPolic%s on MaaS auth surfaces: %s",
			len(conflicts), pluralY(len(conflicts)), strings.Join(names, "; "))
	}
	shouldEmitResolvedEvent := currConflict != nil &&
		currConflict.Status == metav1.ConditionFalse &&
		prevConflict != nil &&
		prevConflict.Status == metav1.ConditionTrue
	if shouldEmitResolvedEvent && r.Recorder != nil {
		r.Recorder.Event(policy, "Normal", "ConflictingAuthPolicyResolved",
			"All conflicting AuthPolicies on MaaS auth surfaces have been resolved")
	}

	// Derive final phase based on model and AuthPolicy health
	phase, message := r.deriveAuthPolicyPhase(policy, missingModels)
	r.updateStatus(ctx, policy, phase, message, statusSnapshot)
	return ctrl.Result{}, nil
}

// findMissingModelRefs returns a list of model refs that don't exist or couldn't be fetched.
// Treats both NotFound and transient errors as "missing" to fail-safe (avoid falsely reporting Active).
func (r *MaaSAuthPolicyReconciler) findMissingModelRefs(ctx context.Context, policy *maasv1alpha1.MaaSAuthPolicy) []maasv1alpha1.ModelRef {
	log := logr.FromContextOrDiscard(ctx)
	var missing []maasv1alpha1.ModelRef
	for _, ref := range policy.Spec.ModelRefs {
		model := &maasv1alpha1.MaaSModelRef{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, model); err != nil {
			// Treat both NotFound and transient errors as missing to fail-safe
			if !apierrors.IsNotFound(err) {
				log.Error(err, "transient error fetching MaaSModelRef, treating as missing", "model", ref.Namespace+"/"+ref.Name)
			}
			missing = append(missing, ref)
		}
	}
	return missing
}

// deriveAuthPolicyPhase determines the MaaSAuthPolicy phase based on model and AuthPolicy health.
func (r *MaaSAuthPolicyReconciler) deriveAuthPolicyPhase(policy *maasv1alpha1.MaaSAuthPolicy, missingModels []maasv1alpha1.ModelRef) (phase maasv1alpha1.Phase, message string) {
	totalModels := len(policy.Spec.ModelRefs)
	missingCount := len(missingModels)
	validModels := totalModels - missingCount

	// All models missing -> Failed
	if validModels == 0 {
		return maasv1alpha1.PhaseFailed, fmt.Sprintf("all %d model references are invalid or missing", totalModels)
	}

	// Check AuthPolicy health for valid models
	var healthyPolicies, unhealthyPolicies int
	for _, ap := range policy.Status.AuthPolicies {
		if ap.Ready {
			healthyPolicies++
		} else {
			unhealthyPolicies++
		}
	}

	// Some models missing -> Degraded
	if missingCount > 0 {
		return maasv1alpha1.PhaseDegraded, fmt.Sprintf("%d of %d model references are missing", missingCount, totalModels)
	}

	// All models valid but some group AuthPolicies unhealthy -> Degraded
	if unhealthyPolicies > 0 {
		return maasv1alpha1.PhaseDegraded, fmt.Sprintf("%d of %d group AuthPolicies not accepted/enforced", unhealthyPolicies, len(policy.Status.AuthPolicies))
	}

	// healthyPolicies == 0 is acceptable: models without subjects are protected solely by the
	// singleton gateway-level AuthPolicy, so no per-model group policy is needed.

	return maasv1alpha1.PhaseActive, "successfully reconciled"
}

type authPolicyRef struct {
	Name           string
	Namespace      string
	Model          string
	ModelNamespace string
}

type modelSubjectAllowlist struct {
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// buildGatewayAuthPolicySpec returns the Authorino AuthPolicy spec for the singleton
// Gateway-level policy. Model identity is resolved dynamically via CEL on every request
// rather than being baked in per-model, so this spec is the same for all MaaSAuthPolicy CRs.
func (r *MaaSAuthPolicyReconciler) buildGatewayAuthPolicySpec(modelAccessJSON string, oidc *oidcConfig, tenantID, tenantName, gatewayNamespace, gatewayName string) map[string]any {
	// Construct tenant-specific maas-api service name using TenantIdentifier
	// Default tenant (tenantID="") uses "maas-api", others use "maas-api-{tenantID}"
	maasAPIServiceName := "maas-api"
	if tenantID != "" {
		maasAPIServiceName = fmt.Sprintf("maas-api-%s", tenantID)
	}

	apiKeyValidationURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/internal/v1/api-keys/validate", maasAPIServiceName, r.MaaSAPINamespace)
	subscriptionSelectorURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/internal/v1/subscriptions/select", maasAPIServiceName, r.MaaSAPINamespace)

	// subscription-info body: same fields as per-model, but requestedModel uses dynamic CEL
	subscriptionInfoBody := fmt.Sprintf(`{
  "groups": %s,
  "username": %s,
  "requestedSubscription": `+celSubscription+`,
  "requestedModel": %s
}`, celGroups, celUsername, celModelIdentity)

	authenticationRules := map[string]any{
		"api-keys": map[string]any{
			"plain": map[string]any{
				"selector": "request.headers.authorization",
			},
			"when": []any{
				map[string]any{
					"selector": "request.headers.authorization",
					"operator": "matches",
					"value":    "^Bearer sk-oai-.*",
				},
			},
			"metrics":  false,
			"priority": int64(0),
		},
		"openshift-identities": map[string]any{
			"kubernetesTokenReview": map[string]any{
				"audiences": []any{r.ClusterAudience},
			},
			"when": []any{
				map[string]any{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
				},
			},
			"metrics":  false,
			"priority": int64(2),
		},
	}

	if oidc != nil {
		authenticationRules["oidc-identities"] = map[string]any{
			"jwt": map[string]any{
				"issuerUrl": oidc.IssuerURL,
				"ttl":       int64(300),
			},
			"when": []any{
				map[string]any{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
				},
			},
			"metrics":  false,
			"priority": int64(1),
		}
	}

	authValidCacheKey := `"api-key|" + request.headers.authorization.replace("Bearer ", "") + "|" + ` + celModelIdentity

	// tenantGatewayIsolationRule is a stub that always allows. It will be replaced with a real
	// maas-api call to verify the API key's tenant matches the gateway hostname when multi-tenant
	// hostname routing is productised (prevents a Coke key from working on a Pepsi gateway).
	tenantGatewayIsolationRule := map[string]any{
		"priority": int64(0),
		"metrics":  false,
		"opa": map[string]any{
			"rego": `# Tenant hostname isolation stub.
# Replace with a real maas-api call to validate that the API key's tenant
# matches the gateway hostname (prevents Coke key on Pepsi gateway).
allow { true }`,
		},
	}

	requireGroupMembershipRego := fmt.Sprintf(`
model_access := %s

request_path := object.get(input.context.request.http, "path", "")
request_headers := object.get(input.context.request.http, "headers", {})

path_parts := [p | p := split(request_path, "/")[_]; p != ""]

path_model_identity := sprintf("%%s/%%s", [path_parts[0], path_parts[1]]) {
	count(path_parts) >= 2
	path_parts[0] != "v1"
	path_parts[0] != "maas-api"
}

raw_header_model_identity := object.get(request_headers, "x-gateway-model-name", "")

header_model_identity := sprintf("%%s/%%s", [split(raw_header_model_identity, "/")[1], split(raw_header_model_identity, "/")[3]]) {
	startswith(raw_header_model_identity, "publishers/")
} else := raw_header_model_identity

model_identity := path_model_identity {
	path_model_identity != ""
} else := header_model_identity {
	header_model_identity != ""
} else := ""

username := input.auth.metadata.apiKeyValidation.username
	{ object.get(input.auth, "metadata", {}).apiKeyValidation.username != "" }
else := input.auth.identity.preferred_username
	{ object.get(input.auth, "identity", {}).preferred_username != "" }
else := input.auth.identity.sub
	{ object.get(input.auth, "identity", {}).sub != "" }
else := input.auth.identity.user.username
	{ object.get(input.auth, "identity", {}).user.username != "" }
else := ""

groups := input.auth.metadata.apiKeyValidation.groups
	{ object.get(input.auth, "metadata", {}).apiKeyValidation.groups != [] }
else := input.auth.identity.groups
	{ object.get(input.auth, "identity", {}).groups != [] }
else := input.auth.identity.user.groups
	{ object.get(input.auth, "identity", {}).user.groups != [] }
else := []

model_rules := object.get(model_access, model_identity, null)

# Management endpoints (e.g. /v1/models, /maas-api/v1/api-keys) carry no model context.
# Allow them here; subscription and rate-limit checks are gated by model-route conditions.
allow {
	model_identity == ""
}

# Inference path: deny by default when no MaaSAuthPolicy covers this model.
# Allow only when the caller's username or a group is explicitly listed.
allow {
	model_rules != null
	model_rules.users[_] == username
}

allow {
	model_rules != null
	g := groups[_]
	model_rules.groups[_] == g
}
`, modelAccessJSON)

	defaultsRules := map[string]any{
		"metadata": map[string]any{
			"apiKeyValidation": map[string]any{
				"when": []any{
					map[string]any{
						"selector": "request.headers.authorization",
						"operator": "matches",
						"value":    "^Bearer sk-oai-.*",
					},
				},
				"http": map[string]any{
					"url":         apiKeyValidationURL,
					"contentType": "application/json",
					"method":      "POST",
					"body": map[string]any{
						"expression": `{"key": request.headers.authorization.replace("Bearer ", "")}`,
					},
				},
				"cache": map[string]any{
					"key": map[string]any{
						"selector": `request.headers.authorization.replace("Bearer ", "")`,
					},
					"ttl": r.MetadataCacheTTL,
				},
				"metrics":  false,
				"priority": int64(0),
			},
			"subscription-info": map[string]any{
				"when": []any{
					map[string]any{
						"predicate": celModelIdentityAvailable,
					},
				},
				"http": map[string]any{
					"url":         subscriptionSelectorURL,
					"contentType": "application/json",
					"method":      "POST",
					"body": map[string]any{
						"expression": subscriptionInfoBody,
					},
				},
				"cache": map[string]any{
					"key": map[string]any{
						"selector": subscriptionGatewayCacheKeySelector(),
					},
					"ttl": r.MetadataCacheTTL,
				},
				"metrics":  false,
				"priority": int64(1),
			},
		},
		"authentication": authenticationRules,
		"authorization": map[string]any{
			"tenant-gateway-isolation": tenantGatewayIsolationRule,
			"auth-valid": map[string]any{
				"metrics":  false,
				"priority": int64(0),
				"opa": map[string]any{
					"rego": `allow {
  object.get(input.auth.metadata, "apiKeyValidation", {})
  input.auth.metadata.apiKeyValidation.valid == true
}
allow {
  not input.auth.metadata.apiKeyValidation
}`,
				},
				"cache": map[string]any{
					"key": map[string]any{
						"selector": authValidCacheKey,
					},
					"ttl": r.authzCacheTTL(),
				},
			},
			"subscription-valid": map[string]any{
				"when": []any{
					map[string]any{
						"predicate": celModelIdentityAvailable,
					},
				},
				"metrics":  false,
				"priority": int64(0),
				"opa": map[string]any{
					"rego": `allow {
	object.get(input.auth.metadata["subscription-info"], "name", "") != ""
	object.get(input.auth.metadata["subscription-info"], "error", "") == ""
	phase := object.get(input.auth.metadata["subscription-info"], "phase", "")
	any([phase == "Active", phase == "Degraded"])
	object.get(input.auth.metadata["subscription-info"], "deletionTimestamp", "") == ""
}`,
				},
				"cache": map[string]any{
					"key": map[string]any{
						"selector": subscriptionGatewayCacheKeySelector(),
					},
					"ttl": r.authzCacheTTL(),
				},
			},
			"require-group-membership": map[string]any{
				"metrics":  false,
				"priority": int64(0),
				"opa": map[string]any{
					"rego": requireGroupMembershipRego,
				},
				"cache": map[string]any{
					"key": map[string]any{
						"selector": gatewayAuthzCacheKeySelector(),
					},
					"ttl": r.authzCacheTTL(),
				},
			},
		},
		"response": map[string]any{
			"success": map[string]any{
				"headers": map[string]any{
					"X-MaaS-Username": map[string]any{
						"when": []any{
							map[string]any{
								"selector": "request.headers.authorization",
								"operator": "matches",
								"value":    "^Bearer sk-oai-.*",
							},
						},
						"plain": map[string]any{
							"selector": "auth.metadata.apiKeyValidation.username",
						},
						"metrics":  false,
						"priority": int64(0),
					},
					"X-MaaS-Username-Token": map[string]any{
						"when": []any{
							map[string]any{
								"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
							},
						},
						"plain": map[string]any{
							"expression": `has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username)`,
						},
						"key":      "X-MaaS-Username",
						"metrics":  false,
						"priority": int64(1),
					},
					"X-MaaS-Group": map[string]any{
						"when": []any{
							map[string]any{
								"selector": "request.headers.authorization",
								"operator": "matches",
								"value":    "^Bearer sk-oai-.*",
							},
						},
						"plain": map[string]any{
							// NOTE: Manual JSON construction without escaping (CEL lacks JSON escape functions).
							// Group names are validated on API key creation to reject quotes/backslashes.
							// Kubernetes group names follow DNS rules (no special chars).
							"expression": `size(auth.metadata.apiKeyValidation.groups) > 0 ? '["' + auth.metadata.apiKeyValidation.groups.join('","') + '"]' : '[]'`,
						},
						"metrics":  false,
						"priority": int64(0),
					},
					"X-MaaS-Group-Token": map[string]any{
						"when": []any{
							map[string]any{
								"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
							},
						},
						"plain": map[string]any{
							"expression": `has(auth.identity.groups) ?` +
								` '["system:authenticated","' + auth.identity.groups.join('","') + '"]'` +
								` : '["' + auth.identity.user.groups.join('","') + '"]'`,
						},
						"key":      "X-MaaS-Group",
						"metrics":  false,
						"priority": int64(1),
					},
					// Only inject X-MaaS-Subscription when there is a real value to inject.
					// An empty string injected for K8s tokens without a subscription header
					// causes maas-api to filter by an empty subscription name and return 0 models.
					// The old maas-api-auth-policy never injected this header for K8s tokens —
					// only for API keys with a non-empty subscription field.
					"X-MaaS-Subscription": map[string]any{
						"when": []any{
							map[string]any{
								"predicate": `(has(auth.metadata) && has(auth.metadata.apiKeyValidation) && auth.metadata.apiKeyValidation.subscription != "") || "x-maas-subscription" in request.headers`,
							},
						},
						"plain": map[string]any{
							"expression": celSubscription,
						},
						"metrics":  false,
						"priority": int64(0),
					},
				},
				"filters": map[string]any{
					"identity": map[string]any{
						"json": map[string]any{
							"properties": map[string]any{
								"groups":     map[string]any{"expression": celGroups},
								"groups_str": map[string]any{"expression": fmt.Sprintf(`(%s).join(",")`, celGroups)},
								"userid": map[string]any{
									"expression": celUsername,
								},
								"keyId": map[string]any{
									"expression": `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ? auth.metadata.apiKeyValidation.keyId : ""`,
								},
								"keyName": map[string]any{
									"expression": `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ? auth.metadata.apiKeyValidation.keyName : ""`,
								},
								"selected_subscription": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"].name : ""`,
								},
								// Model-scoped subscription key: namespace/name@modelIdentity
								// modelIdentity is dynamic (header or path), so this is always current
								"selected_subscription_key": map[string]any{
									"expression": fmt.Sprintf(
										`(has(auth.metadata["subscription-info"].namespace) && `+
											`has(auth.metadata["subscription-info"].name)) `+
											`? auth.metadata["subscription-info"].namespace + "/" `+
											`+ auth.metadata["subscription-info"].name + "@" + %s : ""`,
										celModelIdentity,
									),
								},
								"subscription_info": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"] : {}`,
								},
								"subscription_error": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].error) ? auth.metadata["subscription-info"].error : ""`,
								},
								"subscription_error_message": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].message) ? auth.metadata["subscription-info"].message : ""`,
								},
							},
						},
						"metrics": true, "priority": int64(0),
					},
				},
			},
			"unauthenticated": map[string]any{
				"code": int64(401),
				"message": map[string]any{
					"value": "Authentication required",
				},
			},
			"unauthorized": map[string]any{
				"code": int64(403),
				"body": map[string]any{
					"expression": `has(auth.metadata["subscription-info"].message) ? auth.metadata["subscription-info"].message : "Access denied"`,
				},
				"headers": map[string]any{
					"x-ext-auth-reason": map[string]any{
						"expression": `has(auth.metadata["subscription-info"].error) ? auth.metadata["subscription-info"].error : "unauthorized"`,
					},
					"content-type": map[string]any{
						"value": "text/plain",
					},
				},
			},
		},
	}

	return map[string]any{
		"targetRef": map[string]any{
			"group":     "gateway.networking.k8s.io",
			"kind":      "Gateway",
			"name":      gatewayName,
			"namespace": gatewayNamespace,
		},
		// "when" must live inside "defaults" (not at spec level) because Kuadrant treats
		// top-level "when" as implicit defaults, which conflicts with explicit "defaults".
		"defaults": map[string]any{
			// Skip auth for the health readiness probe so unauthenticated GET /maas-api/health
			// returns 200 without triggering Authorino. Previously handled by maas-api-auth-policy;
			// now that the route-level policy is removed this condition lives at the gateway level.
			"when": []any{
				map[string]any{
					"predicate": `request.path != "/maas-api/health" || request.method != "GET"`,
				},
			},
			"rules": defaultsRules,
		},
	}
}

// reconcileGatewayAuthPolicy creates or updates the singleton Gateway-level AuthPolicy in
// the gateway namespace. All MaaSAuthPolicy reconciliations converge on this one resource.
func (r *MaaSAuthPolicyReconciler) reconcileGatewayAuthPolicy(ctx context.Context, log logr.Logger, modelAccessJSON string, oidc *oidcConfig, tenantID, gatewayNamespace, gatewayName string) error {
	log.Info("reconcileGatewayAuthPolicy entered", "gatewayNamespace", gatewayNamespace, "gatewayName", gatewayName, "tenantID", tenantID)

	// Calculate tenantName from tenantID
	// Default tenant (tenantID="") uses "models-as-a-service", others use tenantID
	tenantName := "models-as-a-service"
	if tenantID != "" {
		tenantName = tenantID
	}

	spec := r.buildGatewayAuthPolicySpec(modelAccessJSON, oidc, tenantID, tenantName, gatewayNamespace, gatewayName)

	// Use legacy name for default gateway (backward compatibility), dynamic name for tenant gateways
	authPolicyName := maasGatewayAuthPolicyName
	if gatewayNamespace != r.GatewayNamespace || gatewayName != r.GatewayName {
		// This is a tenant-specific gateway, use dynamic naming
		authPolicyName = fmt.Sprintf("%s-maas-auth", gatewayName)
	}

	gwPolicy := &unstructured.Unstructured{}
	gwPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	gwPolicy.SetName(authPolicyName)
	gwPolicy.SetNamespace(gatewayNamespace)
	gwPolicy.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-gateway-auth",
		"app.kubernetes.io/component":  "gateway-auth",
	})

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gwPolicy.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(gwPolicy), existing)
	if apierrors.IsNotFound(err) {
		if err := unstructured.SetNestedMap(gwPolicy.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to set gateway AuthPolicy spec: %w", err)
		}
		if err := r.Create(ctx, gwPolicy); err != nil {
			return fmt.Errorf("failed to create gateway AuthPolicy: %w", err)
		}
		log.Info("gateway AuthPolicy created", "name", authPolicyName, "namespace", gatewayNamespace)
		r.deleteGatewayDefaultAuthPolicy(ctx, log)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get gateway AuthPolicy: %w", err)
	}

	if !isManaged(existing) {
		log.Info("gateway AuthPolicy opted out of management, skipping", "name", authPolicyName)
		return nil
	}

	snapshot := existing.DeepCopy()
	if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set gateway AuthPolicy spec for update: %w", err)
	}
	if equality.Semantic.DeepEqual(snapshot.Object, existing.Object) {
		log.Info("gateway AuthPolicy unchanged, skipping update", "name", authPolicyName)
		r.deleteGatewayDefaultAuthPolicy(ctx, log)
		return nil
	}
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update gateway AuthPolicy: %w", err)
	}
	log.Info("gateway AuthPolicy updated", "name", authPolicyName, "namespace", gatewayNamespace)
	r.deleteGatewayDefaultAuthPolicy(ctx, log)
	return nil
}

// reconcileModelAuthPolicies creates or updates the per-model group-membership AuthPolicy for
// each model referenced by the given MaaSAuthPolicy. These lightweight policies use the Kuadrant
// `defaults` strategy so they chain with the singleton gateway-level AuthPolicy without replacing it.
//
// Each per-model policy contains ONLY the require-group-membership authorization rule, which enforces
// the subject allowlist (groups/users) configured via MaaSAuthPolicy.Spec.Subjects. Auth, subscription
// validation, and response shaping are all handled by the singleton gateway-level AuthPolicy.
//
// If a model has no subjects configured across ALL MaaSAuthPolicies that reference it, no per-model
// group policy is created (or the existing one is deleted). The gateway policy alone is sufficient.
func (r *MaaSAuthPolicyReconciler) reconcileModelAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) ([]authPolicyRef, error) {
	var refs []authPolicyRef
	for _, ref := range policy.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, ref.Namespace, ref.Name)
		if err != nil {
			if errors.Is(err, ErrModelNotFound) {
				log.Info("model not found, cleaning up generated AuthPolicy", "model", ref.Namespace+"/"+ref.Name)
				if delErr := r.deleteModelAuthPolicy(ctx, log, ref.Namespace, ref.Name); delErr != nil {
					return nil, fmt.Errorf("failed to clean up AuthPolicy for missing model %s/%s: %w", ref.Namespace, ref.Name, delErr)
				}
				continue
			}
			if errors.Is(err, ErrHTTPRouteNotFound) {
				log.Info("HTTPRoute not found for model, skipping AuthPolicy creation", "model", ref.Namespace+"/"+ref.Name)
				continue
			}
			return nil, fmt.Errorf("failed to resolve HTTPRoute for model %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		// Gateway-level AuthPolicy is the only enforced policy. Remove any legacy per-model
		// group policy to avoid route-level AuthPolicy composition issues on model HTTPRoutes.
		exists, err := r.modelAuthPolicyExists(ctx, httpRouteNS, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to check legacy group policy for model %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if exists {
			if err := r.deleteModelAuthPolicy(ctx, log, httpRouteNS, ref.Name); err != nil {
				return nil, fmt.Errorf("failed to delete legacy group policy for model %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			log.Info("deleted legacy per-model AuthPolicy", "model", ref.Namespace+"/"+ref.Name, "route", httpRouteName)
		} else {
			log.V(1).Info("no legacy per-model AuthPolicy found", "model", ref.Namespace+"/"+ref.Name, "route", httpRouteName)
		}
		log.V(1).Info("gateway policy-only mode: skipping per-model AuthPolicy generation", "model", ref.Namespace+"/"+ref.Name, "route", httpRouteName)
		continue
	}
	if err := r.cleanupStaleAuthPolicies(ctx, log, policy); err != nil {
		return nil, err
	}

	return refs, nil
}

func (r *MaaSAuthPolicyReconciler) aggregateModelSubjectAllowlists(ctx context.Context, policyNamespace string) (map[string]modelSubjectAllowlist, error) {
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(policyNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list MaaSAuthPolicies for gateway aggregation: %w", err)
	}

	aggregate := make(map[string]modelSubjectAllowlist)
	for _, p := range policies.Items {
		if !p.GetDeletionTimestamp().IsZero() {
			continue
		}
		for _, ref := range p.Spec.ModelRefs {
			key := ref.Namespace + "/" + ref.Name
			entry := aggregate[key]
			for _, group := range p.Spec.Subjects.Groups {
				if err := validateCELValue(group.Name, "group name"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s/%s: %w", p.Namespace, p.Name, err)
				}
				entry.Groups = append(entry.Groups, group.Name)
			}
			for _, user := range p.Spec.Subjects.Users {
				if err := validateCELValue(user, "username"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s/%s: %w", p.Namespace, p.Name, err)
				}
				entry.Users = append(entry.Users, user)
			}
			entry.Groups = deduplicateAndSort(entry.Groups)
			entry.Users = deduplicateAndSort(entry.Users)
			aggregate[key] = entry
		}
	}

	return aggregate, nil
}

func (r *MaaSAuthPolicyReconciler) modelAuthPolicyExists(ctx context.Context, modelNamespace, modelName string) (bool, error) {
	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	authPolicyName := fmt.Sprintf("maas-auth-%s", modelName)

	err := r.Get(ctx, types.NamespacedName{Name: authPolicyName, Namespace: modelNamespace}, authPolicy)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// cleanupStaleAuthPolicies deletes aggregated AuthPolicies for models that this
// policy previously contributed to but no longer references in spec.modelRefs.
// Generated AuthPolicies track contributing policies in the
// "maas.opendatahub.io/auth-policies" annotation (namespace-qualified: "ns/name").
func (r *MaaSAuthPolicyReconciler) cleanupStaleAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) error {
	currentModels := make(map[string]bool, len(policy.Spec.ModelRefs))
	for _, ref := range policy.Spec.ModelRefs {
		currentModels[ref.Namespace+"/"+ref.Name] = true
	}

	allManaged := &unstructured.UnstructuredList{}
	allManaged.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"})
	if err := r.List(ctx, allManaged, client.MatchingLabels{
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-auth-policy",
	}); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list managed AuthPolicies for stale cleanup: %w", err)
	}

	for i := range allManaged.Items {
		ap := &allManaged.Items[i]
		modelName := ap.GetLabels()["maas.opendatahub.io/model"]
		if modelName == "" {
			continue
		}
		modelNamespace := ap.GetLabels()["maas.opendatahub.io/model-namespace"]
		if modelNamespace == "" {
			modelNamespace = ap.GetNamespace()
		}
		modelKey := modelNamespace + "/" + modelName
		if currentModels[modelKey] {
			continue
		}
		owners := ap.GetAnnotations()["maas.opendatahub.io/auth-policies"]
		if !annotationListContains(owners, qualifiedName(policy.Namespace, policy.Name)) &&
			!annotationListContains(owners, policy.Name) {
			continue
		}
		log.Info("Cleaning up stale AuthPolicy for removed modelRef", "model", modelKey, "authPolicy", ap.GetName())
		if err := r.deleteModelAuthPolicy(ctx, log, modelNamespace, modelName); err != nil {
			return fmt.Errorf("failed to clean up stale AuthPolicy for removed model %s: %w", modelKey, err)
		}
	}
	return nil
}

// deleteModelAuthPolicy deletes the aggregated AuthPolicy for a model in the given namespace.
func (r *MaaSAuthPolicyReconciler) deleteModelAuthPolicy(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	// Always delete the aggregated AuthPolicy so remaining MaaSAuthPolicies rebuild it
	// without the subjects from the deleted policy. If we skip deletion, the aggregated
	// AuthPolicy will contain stale subjects from the deleted MaaSAuthPolicy.
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-auth-policy",
	}
	if err := r.List(ctx, policyList, client.InNamespace(modelNamespace), labelSelector); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list AuthPolicies for cleanup: %w", err)
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		if labeledModelNamespace := p.GetLabels()["maas.opendatahub.io/model-namespace"]; labeledModelNamespace != "" && labeledModelNamespace != modelNamespace {
			continue
		}
		if !isManaged(p) {
			log.Info("AuthPolicy opted out, skipping deletion", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelNamespace+"/"+modelName)
			continue
		}
		log.Info("Deleting AuthPolicy (no remaining parent policies)", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelNamespace+"/"+modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete AuthPolicy %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}

func (r *MaaSAuthPolicyReconciler) handleDeletion(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		for _, ref := range policy.Spec.ModelRefs {
			log.Info("Deleting model group AuthPolicy so remaining policies can rebuild it", "model", ref.Namespace+"/"+ref.Name)
			if err := r.deleteModelAuthPolicy(ctx, log, ref.Namespace, ref.Name); err != nil {
				log.Error(err, "failed to clean up group AuthPolicy, will retry", "model", ref.Namespace+"/"+ref.Name)
				return ctrl.Result{}, err
			}
		}
		// Also clean up stale group AuthPolicies from modelRefs that were removed
		// before the CR was deleted (edge case: edit + delete before reconcile).
		if err := r.cleanupStaleAuthPolicies(ctx, log, policy); err != nil {
			return ctrl.Result{}, err
		}

		// If this is the last MaaSAuthPolicy, also delete the singleton gateway-level AuthPolicy.
		remaining := &maasv1alpha1.MaaSAuthPolicyList{}
		if err := r.List(ctx, remaining); err != nil {
			log.Error(err, "failed to list remaining MaaSAuthPolicies for gateway cleanup check")
			return ctrl.Result{}, err
		}
		// Count policies not being deleted and not the current one
		liveCount := 0
		for _, p := range remaining.Items {
			if p.Name == policy.Name && p.Namespace == policy.Namespace {
				continue
			}
			if p.GetDeletionTimestamp().IsZero() {
				liveCount++
			}
		}
		if liveCount == 0 {
			if err := r.deleteGatewayAuthPolicy(ctx, log, policy.Namespace); err != nil {
				log.Error(err, "failed to delete gateway AuthPolicy")
				return ctrl.Result{}, err
			}
			if err := r.ensureGatewayDefaultAuthPolicy(ctx, log); err != nil {
				log.Error(err, "failed to restore gateway-default-auth")
				return ctrl.Result{}, err
			}
		}

		controllerutil.RemoveFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// deleteGatewayAuthPolicy removes the tenant's Gateway-level AuthPolicy when no
// MaaSAuthPolicy CRs remain in that tenant namespace.
func (r *MaaSAuthPolicyReconciler) deleteGatewayAuthPolicy(ctx context.Context, log logr.Logger, tenantNamespace string) error {
	// Get tenant's gateway info
	gatewayNs, gatewayName, err := r.fetchGatewayInfo(ctx, log, tenantNamespace)
	if err != nil {
		return fmt.Errorf("failed to fetch gateway info for deletion: %w", err)
	}

	// Use legacy name for default gateway (backward compatibility), dynamic name for tenant gateways
	authPolicyName := maasGatewayAuthPolicyName
	if gatewayNs != r.GatewayNamespace || gatewayName != r.GatewayName {
		// This is a tenant-specific gateway, use dynamic naming
		authPolicyName = fmt.Sprintf("%s-maas-auth", gatewayName)
	}

	gwPolicy := &unstructured.Unstructured{}
	gwPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	gwPolicy.SetName(authPolicyName)
	gwPolicy.SetNamespace(gatewayNs)

	if err := r.Delete(ctx, gwPolicy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete gateway AuthPolicy %s/%s: %w", gatewayNs, authPolicyName, err)
	}
	log.Info("gateway AuthPolicy deleted (no remaining MaaSAuthPolicies)", "name", authPolicyName, "namespace", gatewayNs, "tenantNamespace", tenantNamespace)
	return nil
}

// deleteGatewayDefaultAuthPolicy removes the static deny-all gateway-default-auth policy
// so it does not conflict with the dynamic maas-gateway-auth policy on the same Gateway.
func (r *MaaSAuthPolicyReconciler) deleteGatewayDefaultAuthPolicy(ctx context.Context, log logr.Logger) {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	policy.SetName(gatewayDefaultAuthPolicyName)
	policy.SetNamespace(r.GatewayNamespace)

	if err := r.Delete(ctx, policy); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete gateway-default-auth (non-fatal)", "name", gatewayDefaultAuthPolicyName)
		}
		return
	}
	log.Info("deleted gateway-default-auth (superseded by maas-gateway-auth)", "name", gatewayDefaultAuthPolicyName, "namespace", r.GatewayNamespace)
}

// ensureGatewayDefaultAuthPolicy recreates the static deny-all gateway-default-auth policy
// after the last MaaSAuthPolicy is removed, so unconfigured model routes remain denied.
func (r *MaaSAuthPolicyReconciler) ensureGatewayDefaultAuthPolicy(ctx context.Context, log logr.Logger) error {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	policy.SetName(gatewayDefaultAuthPolicyName)
	policy.SetNamespace(r.GatewayNamespace)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(policy.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(policy), existing); err == nil {
		log.V(1).Info("gateway-default-auth already exists, skipping recreation", "name", gatewayDefaultAuthPolicyName)
		return nil
	}

	policy.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-controller",
		"app.kubernetes.io/component":  "default-policy",
	})
	spec := map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  r.GatewayName,
		},
		"defaults": map[string]any{
			"rules": map[string]any{
				"authentication": map[string]any{},
				"authorization": map[string]any{
					"deny-unconfigured-models": map[string]any{
						"metrics":  false,
						"priority": int64(0),
						"patternMatching": map[string]any{
							"patterns": []any{
								map[string]any{
									"operator": "eq",
									"selector": "context.request.http.method",
									"value":    "__deny_unconfigured_models__",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := unstructured.SetNestedMap(policy.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set gateway-default-auth spec: %w", err)
	}
	if err := r.Create(ctx, policy); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create gateway-default-auth: %w", err)
	}
	log.Info("restored gateway-default-auth (no remaining MaaSAuthPolicies)", "name", gatewayDefaultAuthPolicyName, "namespace", r.GatewayNamespace)
	return nil
}

func (r *MaaSAuthPolicyReconciler) updateAuthPolicyRefStatus(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy, refs []authPolicyRef) {
	policy.Status.AuthPolicies = make([]maasv1alpha1.AuthPolicyRefStatus, 0, len(refs))
	for _, ref := range refs {
		ap := &unstructured.Unstructured{}
		ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		ap.SetNamespace(ref.Namespace)
		ap.SetName(ref.Name)

		status := maasv1alpha1.AuthPolicyRefStatus{
			ResourceRefStatus: maasv1alpha1.ResourceRefStatus{
				Name:      ref.Name,
				Namespace: ref.Namespace,
			},
			Model:          ref.Model,
			ModelNamespace: ref.ModelNamespace,
		}

		if err := r.Get(ctx, client.ObjectKeyFromObject(ap), ap); err != nil {
			log.Info("could not get AuthPolicy for status", "name", ref.Name, "namespace", ref.Namespace, "error", err)
			status.Ready = false
			if apierrors.IsNotFound(err) {
				status.Reason = maasv1alpha1.ReasonNotFound
				status.Message = "AuthPolicy not created yet"
			} else {
				status.Reason = maasv1alpha1.ReasonGetFailed
				status.Message = fmt.Sprintf("failed to get AuthPolicy: %v", err)
			}
			policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, status)
			continue
		}

		ready, reason, message := getAuthPolicyReadyState(ap)
		status.Ready = ready
		status.Reason = reason
		status.Message = message
		policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, status)
	}
}

// getAuthPolicyReadyState checks if an AuthPolicy is accepted and enforced.
// Returns ready=true only if both Accepted and Enforced conditions are True.
func getAuthPolicyReadyState(ap *unstructured.Unstructured) (ready bool, reason maasv1alpha1.ConditionReason, message string) {
	conditions, found, err := unstructured.NestedSlice(ap.Object, "status", "conditions")
	if err != nil || !found || len(conditions) == 0 {
		return false, maasv1alpha1.ReasonConditionsNotFound, "status conditions not available"
	}

	var accepted, enforced bool
	var acceptedMsg, enforcedMsg string

	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		msg, _ := cond["message"].(string)

		switch typ {
		case "Accepted":
			accepted = status == "True"
			if !accepted {
				acceptedMsg = msg
			}
		case "Enforced":
			enforced = status == "True"
			if !enforced {
				enforcedMsg = msg
			}
		}
	}

	if accepted && enforced {
		return true, maasv1alpha1.ReasonAcceptedEnforced, ""
	}
	if !accepted {
		return false, maasv1alpha1.ReasonNotAccepted, acceptedMsg
	}
	return false, maasv1alpha1.ReasonNotEnforced, enforcedMsg
}

func (r *MaaSAuthPolicyReconciler) updateStatus(ctx context.Context, policy *maasv1alpha1.MaaSAuthPolicy, phase maasv1alpha1.Phase, message string, statusSnapshot *maasv1alpha1.MaaSAuthPolicyStatus) {
	policy.Status.Phase = phase

	var status metav1.ConditionStatus
	var reason maasv1alpha1.ConditionReason
	switch phase {
	case maasv1alpha1.PhaseActive:
		status = metav1.ConditionTrue
		reason = maasv1alpha1.ReasonReconciled
	case maasv1alpha1.PhaseDegraded:
		status = metav1.ConditionFalse
		reason = maasv1alpha1.ReasonPartialFailure
	case maasv1alpha1.PhaseFailed:
		status = metav1.ConditionFalse
		reason = maasv1alpha1.ReasonReconcileFailed
	case maasv1alpha1.PhaseInvalid:
		status = metav1.ConditionFalse
		reason = maasv1alpha1.ReasonInvalidSpec
	default:
		status = metav1.ConditionUnknown
		reason = maasv1alpha1.ReasonUnknown
	}

	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: policy.GetGeneration(),
	})

	if equality.Semantic.DeepEqual(*statusSnapshot, policy.Status) {
		return
	}

	if err := r.Status().Update(ctx, policy); err != nil {
		log := logr.FromContextOrDiscard(ctx)
		log.Error(err, "failed to update MaaSAuthPolicy status", "name", policy.Name)
	}
}

// ValidateCacheTTLs validates that cache TTL configuration is valid.
// Returns an error if either TTL is negative (fail-closed validation).
func (r *MaaSAuthPolicyReconciler) ValidateCacheTTLs() error {
	if r.MetadataCacheTTL < 0 {
		return fmt.Errorf("metadata cache TTL must be non-negative, got %d", r.MetadataCacheTTL)
	}
	if r.AuthzCacheTTL < 0 {
		return fmt.Errorf("authorization cache TTL must be non-negative, got %d", r.AuthzCacheTTL)
	}
	return nil
}

func (r *MaaSAuthPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Validate cache TTL configuration
	log := ctrl.Log.WithName("maas-authpolicy-controller")

	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("maas-authpolicy-controller")
	}

	// Reject negative TTL values
	if err := r.ValidateCacheTTLs(); err != nil {
		return err
	}

	if r.AuthzCacheTTL > r.MetadataCacheTTL {
		log.Info("WARNING: Authorization cache TTL exceeds metadata cache TTL. "+
			"Authorization caches will be capped at metadata TTL to prevent stale authorization decisions.",
			"authzCacheTTL", r.AuthzCacheTTL,
			"metadataCacheTTL", r.MetadataCacheTTL,
			"effectiveAuthzTTL", r.authzCacheTTL())
	}

	// Watch generated AuthPolicies so we re-reconcile when someone manually edits them.
	generatedAuthPolicy := &unstructured.Unstructured{}
	generatedAuthPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})

	// Watch Tenant so we re-reconcile when OIDC configuration changes.
	tenant := &unstructured.Unstructured{}
	tenant.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "maas.opendatahub.io",
		Version: "v1alpha1",
		Kind:    "Tenant",
	})

	b := ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSAuthPolicy{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.Funcs{UpdateFunc: deletionTimestampSet},
		))).
		// Watch HTTPRoutes so we re-reconcile when KServe creates/updates a route
		// (fixes race condition where MaaSAuthPolicy is created before HTTPRoute exists).
		Watches(&gatewayapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(
			r.mapHTTPRouteToMaaSAuthPolicies,
		)).
		// Watch MaaSModelRefs so we re-reconcile when a model is created or deleted.
		Watches(&maasv1alpha1.MaaSModelRef{}, handler.EnqueueRequestsFromMapFunc(
			r.mapMaaSModelRefToMaaSAuthPolicies,
		)).
		// Watch generated AuthPolicies so manual edits get overwritten by the controller.
		Watches(generatedAuthPolicy, handler.EnqueueRequestsFromMapFunc(
			r.mapGeneratedAuthPolicyToParent,
		)).
		// Watch Tenant so OIDC configuration changes trigger reconciles.
		Watches(tenant, handler.EnqueueRequestsFromMapFunc(
			r.mapTenantToMaaSAuthPolicies,
		))
	if r.TenantNamespaceDiscoveryEnabled {
		// Watch Namespaces so that policies in newly labeled tenant
		// namespaces are discovered without a controller restart.
		b = b.Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(
			r.mapNamespaceToMaaSAuthPolicies,
		), builder.WithPredicates(predicate.LabelChangedPredicate{}))
	}
	return b.Complete(r)
}

// mapTenantToMaaSAuthPolicies enqueues MaaSAuthPolicy resources in the same
// namespace as the changed Tenant so that OIDC configuration changes propagate
// only to the affected tenant's policies.
func (r *MaaSAuthPolicyReconciler) mapTenantToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	policyList := &maasv1alpha1.MaaSAuthPolicyList{}
	if err := r.List(ctx, policyList, client.InNamespace(obj.GetNamespace())); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list MaaSAuthPolicy resources for Tenant change",
			"tenantNamespace", obj.GetNamespace())
		return nil
	}
	policyList.Items = filterAuthPoliciesByTenantNamespace(ctx, r.Client, policyList.Items, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)

	requests := make([]reconcile.Request, len(policyList.Items))
	for i, policy := range policyList.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      policy.Name,
				Namespace: policy.Namespace,
			},
		}
	}
	return requests
}

// mapNamespaceToMaaSAuthPolicies enqueues all MaaSAuthPolicy resources in a
// namespace when that namespace's labels change (e.g. AITenant label added or removed).
func (r *MaaSAuthPolicyReconciler) mapNamespaceToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetName()
	if ns != r.TenantNamespace && !r.TenantNamespaceDiscoveryEnabled {
		return nil
	}
	policyList := &maasv1alpha1.MaaSAuthPolicyList{}
	if err := r.List(ctx, policyList, client.InNamespace(ns)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list MaaSAuthPolicy for namespace label change", "namespace", ns)
		return nil
	}
	requests := make([]reconcile.Request, len(policyList.Items))
	for i, p := range policyList.Items {
		requests[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace}}
	}
	return requests
}

func (r *MaaSAuthPolicyReconciler) findAnyAuthPolicyForModel(ctx context.Context, modelNamespace, modelName string) *maasv1alpha1.MaaSAuthPolicy {
	policies, err := findAllAuthPoliciesForModel(ctx, r.Client, modelNamespace, modelName)
	if err != nil {
		return nil
	}
	policies = filterAuthPoliciesByTenantNamespace(ctx, r.Client, policies, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)
	if len(policies) == 0 {
		return nil
	}
	return &policies[0]
}

// mapGeneratedAuthPolicyToParent maps a generated AuthPolicy back to any
// MaaSAuthPolicy that references the same model. The AuthPolicy is per-model
// (aggregated), so we use the model label to find a policy to trigger reconciliation.
func (r *MaaSAuthPolicyReconciler) mapGeneratedAuthPolicyToParent(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "maas-controller" {
		return nil
	}
	modelName := labels["maas.opendatahub.io/model"]
	if modelName == "" {
		return nil
	}
	modelNamespace := labels["maas.opendatahub.io/model-namespace"]
	if modelNamespace == "" {
		modelNamespace = obj.GetNamespace()
	}
	ap := r.findAnyAuthPolicyForModel(ctx, modelNamespace, modelName)
	if ap == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: ap.Name, Namespace: ap.Namespace},
	}}
}

// mapMaaSModelRefToMaaSAuthPolicies returns reconcile requests for all MaaSAuthPolicies
// that reference the given MaaSModelRef.
func (r *MaaSAuthPolicyReconciler) mapMaaSModelRefToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	model, ok := obj.(*maasv1alpha1.MaaSModelRef)
	if !ok {
		return nil
	}
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil
	}
	policies.Items = filterAuthPoliciesByTenantNamespace(ctx, r.Client, policies.Items, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)
	var requests []reconcile.Request
	for _, p := range policies.Items {
		for _, ref := range p.Spec.ModelRefs {
			if ref.Namespace == model.Namespace && ref.Name == model.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// mapHTTPRouteToMaaSAuthPolicies returns reconcile requests for all MaaSAuthPolicies
// that reference models in the HTTPRoute's namespace.
func (r *MaaSAuthPolicyReconciler) mapHTTPRouteToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayapiv1.HTTPRoute)
	if !ok {
		return nil
	}
	// Find MaaSModelRefs in this namespace
	var models maasv1alpha1.MaaSModelRefList
	if err := r.List(ctx, &models, client.InNamespace(route.Namespace)); err != nil {
		return nil
	}
	// Use namespace-qualified keys to prevent cross-namespace matches
	modelKeysInNS := map[string]bool{}
	for _, m := range models.Items {
		modelKeysInNS[m.Namespace+"/"+m.Name] = true
	}
	if len(modelKeysInNS) == 0 {
		return nil
	}
	// Find MaaSAuthPolicies that reference any of these models
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil
	}
	policies.Items = filterAuthPoliciesByTenantNamespace(ctx, r.Client, policies.Items, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)
	var requests []reconcile.Request
	for _, p := range policies.Items {
		for _, ref := range p.Spec.ModelRefs {
			if modelKeysInNS[ref.Namespace+"/"+ref.Name] {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// deduplicateAndSort removes duplicates from a string slice and sorts it.
// This ensures stable output across reconciles, preventing spurious updates
// caused by non-deterministic Kubernetes List order.
func deduplicateAndSort(items []string) []string {
	if len(items) == 0 {
		return items
	}
	// Use a map to deduplicate
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item] = true
	}
	// Build deduplicated slice
	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	// Sort for deterministic output
	sort.Strings(result)
	return result
}
