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
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// MaaSAuthPolicyReconciler reconciles a MaaSAuthPolicy object
type MaaSAuthPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// MaaSAPINamespace is the namespace where maas-api service is deployed.
	// Used to construct the subscription selector endpoint URL.
	MaaSAPINamespace string

	// GatewayName is the name of the Gateway used for model HTTPRoutes (configurable via flags).
	GatewayName string

	// ClusterAudience is the OIDC audience of the cluster (configurable via flags).
	// Standard clusters use "https://kubernetes.default.svc"; HyperShift/ROSA use a custom OIDC provider URL.
	ClusterAudience string

	// MetadataCacheTTL is the TTL in seconds for Authorino metadata HTTP caching.
	// Applies to apiKeyValidation and subscription-info metadata evaluators.
	MetadataCacheTTL int64

	// AuthzCacheTTL is the TTL in seconds for Authorino OPA authorization caching.
	// Applies to auth-valid, subscription-valid, and require-group-membership authorization evaluators.
	AuthzCacheTTL int64
}

func (r *MaaSAuthPolicyReconciler) clusterAudience() string {
	if r.ClusterAudience != "" {
		return r.ClusterAudience
	}
	return defaultClusterAudience
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

// CEL sub-expressions reused across Authorino cache-key selectors.
const (
	celUserID = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.userId : auth.identity.user.username`
	celGroups = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.groups : auth.identity.user.groups`
	celSubscription = `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ` +
		`? auth.metadata.apiKeyValidation.subscription : ` +
		`("x-maas-subscription" in request.headers ? request.headers["x-maas-subscription"] : "")`
)

// subscriptionCacheKeySelector builds the CEL cache-key expression for subscription-info
// and subscription-valid evaluators: "userId|groups|subscription|namespace/name".
func subscriptionCacheKeySelector(ns, name string) string {
	return fmt.Sprintf(
		`(%s) + "|" + (%s).join(",") + "|" + (%s) + "|%s/%s"`,
		celUserID, celGroups, celSubscription, ns, name,
	)
}

// authzCacheKeySelector builds the CEL cache-key expression for authorization evaluators
// (require-group-membership): "userId|groups|namespace/name".
func authzCacheKeySelector(ns, name string) string {
	return fmt.Sprintf(
		`(%s) + "|" + (%s).join(",") + "|%s/%s"`,
		celUserID, celGroups, ns, name,
	)
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
//+kubebuilder:rbac:groups=config.openshift.io,resources=authentications,verbs=get

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

	if !policy.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, policy)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		controllerutil.AddFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	statusSnapshot := policy.Status.DeepCopy()

	refs, err := r.reconcileModelAuthPolicies(ctx, log, policy)
	if err != nil {
		log.Error(err, "failed to reconcile model AuthPolicies")
		r.updateStatus(ctx, policy, "Failed", fmt.Sprintf("Failed to reconcile: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	r.updateAuthPolicyRefStatus(ctx, log, policy, refs)
	r.updateStatus(ctx, policy, "Active", "Successfully reconciled", statusSnapshot)
	return ctrl.Result{}, nil
}

type authPolicyRef struct {
	Name           string
	Namespace      string
	Model          string
	ModelNamespace string
}

func (r *MaaSAuthPolicyReconciler) reconcileModelAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) ([]authPolicyRef, error) {
	var refs []authPolicyRef
	// Model-centric approach: for each model referenced by this auth policy,
	// find ALL auth policies for that model and build a single aggregated AuthPolicy.
	// Kuadrant only allows one AuthPolicy per HTTPRoute target.
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
				// HTTPRoute doesn't exist yet - skip for now. HTTPRoute watch will trigger reconciliation when route is created.
				log.Info("HTTPRoute not found for model, skipping AuthPolicy creation", "model", ref.Namespace+"/"+ref.Name)
				continue
			}
			return nil, fmt.Errorf("failed to resolve HTTPRoute for model %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		// Validate model namespace and name for CEL injection prevention
		if err := validateCELValue(ref.Namespace, "model namespace"); err != nil {
			return nil, fmt.Errorf("invalid model namespace in modelRef %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		if err := validateCELValue(ref.Name, "model name"); err != nil {
			return nil, fmt.Errorf("invalid model name in modelRef %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		// Find ALL auth policies for this model (not just the current one)
		allPolicies, err := findAllAuthPoliciesForModel(ctx, r.Client, ref.Namespace, ref.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to list auth policies for model %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		// Aggregate allowed groups and users from ALL auth policies
		// Will be checked in OPA policy that handles both API keys and K8s tokens
		// Initialize as empty slices (not nil) so json.Marshal produces [] instead of null
		allowedGroups := []string{}
		allowedUsers := []string{}
		var policyNames []string
		for _, ap := range allPolicies {
			policyNames = append(policyNames, ap.Name)
			for _, group := range ap.Spec.Subjects.Groups {
				if err := validateCELValue(group.Name, "group name"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s: %w", ap.Name, err)
				}
				allowedGroups = append(allowedGroups, group.Name)
			}
			for _, user := range ap.Spec.Subjects.Users {
				if err := validateCELValue(user, "username"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s: %w", ap.Name, err)
				}
				allowedUsers = append(allowedUsers, user)
			}
		}

		// Deduplicate and sort to ensure stable output across reconciles
		// (Kubernetes List order is not guaranteed to be deterministic)
		policyNames = deduplicateAndSort(policyNames)
		allowedGroups = deduplicateAndSort(allowedGroups)
		allowedUsers = deduplicateAndSort(allowedUsers)

		// Construct API URLs using configured namespace
		apiKeyValidationURL := fmt.Sprintf("https://maas-api.%s.svc.cluster.local:8443/internal/v1/api-keys/validate", r.MaaSAPINamespace)
		subscriptionSelectorURL := fmt.Sprintf("https://maas-api.%s.svc.cluster.local:8443/internal/v1/subscriptions/select", r.MaaSAPINamespace)

		rule := map[string]any{
			"metadata": map[string]any{
				// API Key Validation - validates the API key and returns user identity + groups
				// Only runs for API key requests (sk-oai-* prefix), not K8s tokens
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
					// Cache API key validation results keyed by the API key itself.
					// Key format: "api-key-value"
					// This prevents repeated validation calls for the same API key within the TTL window.
					"cache": map[string]any{
						"key": map[string]any{
							"selector": `request.headers.authorization.replace("Bearer ", "")`,
						},
						"ttl": r.MetadataCacheTTL,
					},
					"metrics":  false,
					"priority": int64(0),
				},
				// Resolve subscription via maas-api
				// For API keys: uses subscription bound to the key at mint time
				// For K8s tokens: uses X-MaaS-Subscription header if provided, otherwise finds all accessible
				// Priority 1 ensures this runs after apiKeyValidation (priority 0).
				"subscription-info": map[string]any{
					"http": map[string]any{
						"url":         subscriptionSelectorURL,
						"contentType": "application/json",
						"method":      "POST",
						"body": map[string]any{
							"expression": fmt.Sprintf(`{
  "groups": (has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ? auth.metadata.apiKeyValidation.groups : auth.identity.user.groups,
  "username": (has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ? auth.metadata.apiKeyValidation.username : auth.identity.user.username,
  "requestedSubscription": `+celSubscription+`,
  "requestedModel": "%s/%s"
}`, ref.Namespace, ref.Name),
						},
					},
					// Cache subscription selection results keyed by user ID, groups, requested subscription, and model.
					// Each model has its own cache entry since subscription validation is model-specific.
					// Key format: "userId|groups|requested-subscription|model-namespace/model-name"
					// For API keys: userId is database-assigned UUID (collision-resistant)
					// For K8s tokens: userId is validated username (system:serviceaccount:namespace:sa-name)
					// Groups are joined with commas to create a stable string representation.
					"cache": map[string]any{
						"key": map[string]any{
							"selector": subscriptionCacheKeySelector(ref.Namespace, ref.Name),
						},
						"ttl": r.MetadataCacheTTL,
					},
					"metrics":  false,
					"priority": int64(1),
				},
			},
			"authentication": map[string]any{
				// API Keys - plain authentication, actual validation in metadata layer
				// Only processes tokens with sk-oai- prefix (OpenAI-compatible API keys)
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
				// Kubernetes/OpenShift tokens - validated via TokenReview API
				// Only enabled for /v1/models endpoint (read-only model listing)
				// Inferencing endpoints require API keys for billing/tracking
				// The api-keys authentication (priority 0) runs first and will consume API key requests,
				// so we don't need to explicitly exclude them here
				"kubernetes-tokens": map[string]any{
					"kubernetesTokenReview": map[string]any{
						"audiences": []any{r.clusterAudience()},
					},
					"when": []any{
						map[string]any{
							"selector": "request.url_path",
							"operator": "matches",
							"value":    ".*/v1/models$",
						},
						map[string]any{
							"selector": "request.headers.authorization",
							"operator": "neq",
							"value":    "",
						},
					},
					"metrics":  false,
					"priority": int64(1),
				},
			},
		}

		// Build authorization rules
		authRules := make(map[string]any)

		// Validate authentication: API key must be valid, OR K8s token must be authenticated
		// For API keys: check apiKeyValidation.valid == true (boolean)
		// For K8s tokens: check that identity.username exists (TokenReview succeeded)
		authRules["auth-valid"] = map[string]any{
			"metrics":  false,
			"priority": int64(0),
			"opa": map[string]any{
				"rego": `# API key authentication: validate the key
allow {
  object.get(input.auth.metadata, "apiKeyValidation", {})
  input.auth.metadata.apiKeyValidation.valid == true
}

# Kubernetes token authentication: check identity exists
allow {
  object.get(input.auth.identity, "user", {}).username != ""
}`,
			},
			// Cache authorization result keyed by authentication source and identity.
			// For API keys: uses the API key value
			// For K8s tokens: uses the username
			// Key format: "auth-type|identity|model"
			// TTL cannot exceed metadata TTL (auth-valid depends on apiKeyValidation metadata)
			"cache": map[string]any{
				"key": map[string]any{
					"selector": fmt.Sprintf(`(has(auth.metadata.apiKeyValidation) ? "api-key|" + request.headers.authorization.replace("Bearer ", "") : "k8s-token|" + auth.identity.user.username) + "|%s/%s"`, ref.Namespace, ref.Name),
				},
				"ttl": r.authzCacheTTL(),
			},
		}

		// Fail-close: require successful subscription selection (name must be present)
		authRules["subscription-valid"] = map[string]any{
			"metrics":  false,
			"priority": int64(0),
			"opa": map[string]any{
				"rego": `allow { object.get(input.auth.metadata["subscription-info"], "name", "") != "" }`,
			},
			// Cache authorization result keyed by subscription selection inputs.
			// Uses same key dimensions as subscription-info metadata to ensure cache coherence.
			// Key format: "userId|groups|requested-subscription|model"
			// For API keys: userId is database UUID. For K8s tokens: validated username.
			// TTL cannot exceed metadata TTL (subscription-valid depends on subscription-info metadata)
			"cache": map[string]any{
				"key": map[string]any{
					"selector": subscriptionCacheKeySelector(ref.Namespace, ref.Name),
				},
				"ttl": r.authzCacheTTL(),
			},
		}

		// Build aggregated authorization rule from ALL auth policies' subjects
		// Uses OPA to check membership for both API keys and K8s tokens
		if len(allowedGroups) > 0 || len(allowedUsers) > 0 {
			groupsJSON, err := json.Marshal(allowedGroups)
			if err != nil {
				return nil, fmt.Errorf("marshal allowedGroups: %w", err)
			}
			usersJSON, err := json.Marshal(allowedUsers)
			if err != nil {
				return nil, fmt.Errorf("marshal allowedUsers: %w", err)
			}
			authRules["require-group-membership"] = map[string]any{
				"metrics":  false,
				"priority": int64(0),
				"opa": map[string]any{
					"rego": fmt.Sprintf(`
# Allowed groups and users from all MaaSAuthPolicies
allowed_groups := %s
allowed_users := %s

# Extract username from API key or K8s token
username := input.auth.metadata.apiKeyValidation.username
    { object.get(input.auth, "metadata", {}).apiKeyValidation.username != "" }
else := input.auth.identity.user.username
    { object.get(input.auth, "identity", {}).user.username != "" }
else := ""

# Extract groups from API key or K8s token
groups := input.auth.metadata.apiKeyValidation.groups
    { object.get(input.auth, "metadata", {}).apiKeyValidation.groups != [] }
else := input.auth.identity.user.groups
    { object.get(input.auth, "identity", {}).user.groups != [] }
else := []

# Allow if user is in allowed users
allow {
    username == allowed_users[_]
}

# Allow if any user group is in allowed groups
allow {
    groups[_] == allowed_groups[_]
}
`, string(groupsJSON), string(usersJSON)),
				},
				// Cache authorization result keyed by user ID, groups, and model.
				// The allowed groups/users are baked into the OPA rego, so the cache is per-model-policy.
				// Key format: "userId|groups|model"
				// For API keys: userId is database UUID. For K8s tokens: validated username.
				// TTL cannot exceed metadata TTL (require-group-membership depends on apiKeyValidation metadata for groups)
				"cache": map[string]any{
					"key": map[string]any{
						"selector": authzCacheKeySelector(ref.Namespace, ref.Name),
					},
					"ttl": r.authzCacheTTL(),
				},
			}
		}

		if len(authRules) > 0 {
			rule["authorization"] = authRules
		}

		// Pass ALL user groups unfiltered in the response so TokenRateLimitPolicy predicates can
		// match against subscription groups (which may differ from auth policy groups).
		// Also inject subscription metadata from subscription-info for Limitador metrics.
		// For API keys: username/groups come from apiKeyValidation metadata
		// Identity headers intentionally removed for defense-in-depth:
		// User identity, groups, and key IDs are not forwarded to upstream model workloads
		// to prevent accidental disclosure in logs or dumps. All identity information remains
		// available to TRLP and telemetry via auth.identity and filters.identity below.
		// Exception: X-MaaS-Subscription is injected for Istio Telemetry (per-subscription latency tracking).
		rule["response"] = map[string]any{
			"success": map[string]any{
				"headers": map[string]any{
					// Strip Authorization header to prevent token exfiltration to model backends
					// Both API keys and OpenShift tokens are validated by Authorino, but should
					// not be forwarded to model services to prevent credential theft
					"Authorization": map[string]any{
						"plain": map[string]any{
							"value": "",
						},
						"key":      "authorization",
						"metrics":  false,
						"priority": int64(0),
					},
					// Subscription bound to API key (only for API keys)
					// For K8s tokens, this header is not injected (empty string)
					"X-MaaS-Subscription": map[string]any{
						"plain": map[string]any{
							"expression": `(has(auth.metadata) && has(auth.metadata.apiKeyValidation)) ? auth.metadata.apiKeyValidation.subscription : ""`,
						},
						"metrics":  false,
						"priority": int64(0),
					},
				},
				"filters": map[string]any{
					"identity": map[string]any{
						"json": map[string]any{
							"properties": map[string]any{
								"groups":     map[string]any{"expression": "auth.metadata.apiKeyValidation.groups"},
								"groups_str": map[string]any{"expression": `auth.metadata.apiKeyValidation.groups.join(",")`},
								"userid": map[string]any{
									"selector": "auth.metadata.apiKeyValidation.username",
								},
								"keyId": map[string]any{
									"selector": "auth.metadata.apiKeyValidation.keyId",
								},
								// Subscription metadata from /internal/v1/subscriptions/select endpoint
								"selected_subscription": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"].name : ""`,
								},
								// Model-scoped subscription key for TRLP isolation: namespace/name@modelNamespace/modelName
								"selected_subscription_key": map[string]any{
									"expression": fmt.Sprintf(
										`has(auth.metadata["subscription-info"].namespace) && `+
											`has(auth.metadata["subscription-info"].name) `+
											`? auth.metadata["subscription-info"].namespace + "/" `+
											`+ auth.metadata["subscription-info"].name + "@%s/%s" : ""`,
										ref.Namespace, ref.Name,
									),
								},
								// Full subscription-info object from subscription-select endpoint
								// Contains: name, namespace, labels, organizationId, costCenter, error, message
								// Consumers should access nested fields (e.g., subscription_info.organizationId)
								"subscription_info": map[string]any{
									"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"] : {}`,
								},
								// Error information (for debugging - only populated when selection fails)
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
			// Custom denial responses that include subscription error details
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
		}

		// Build the aggregated AuthPolicy (one per model, covering all MaaSAuthPolicies)
		authPolicyName := fmt.Sprintf("maas-auth-%s", ref.Name)
		authPolicy := &unstructured.Unstructured{}
		authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		authPolicy.SetName(authPolicyName)
		authPolicy.SetNamespace(httpRouteNS)
		authPolicy.SetLabels(map[string]string{
			"maas.opendatahub.io/model":    ref.Name,
			"app.kubernetes.io/managed-by": "maas-controller",
			"app.kubernetes.io/part-of":    "maas-auth-policy",
			"app.kubernetes.io/component":  "auth-policy",
		})
		authPolicy.SetAnnotations(map[string]string{
			"maas.opendatahub.io/auth-policies": strings.Join(policyNames, ","),
		})

		refs = append(refs, authPolicyRef{Name: authPolicyName, Namespace: httpRouteNS, Model: ref.Name, ModelNamespace: ref.Namespace})

		spec := map[string]any{
			"targetRef": map[string]any{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
				"name":  httpRouteName,
			},
			"rules": rule,
		}
		if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}

		// Create or update AuthPolicy
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
		err = r.Get(ctx, client.ObjectKeyFromObject(authPolicy), existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, authPolicy); err != nil {
				return nil, fmt.Errorf("failed to create AuthPolicy for model %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			log.Info("AuthPolicy created", "name", authPolicyName, "model", ref.Namespace+"/"+ref.Name, "policies", policyNames)
		} else if err != nil {
			return nil, fmt.Errorf("failed to get existing AuthPolicy: %w", err)
		} else {
			if !isManaged(existing) {
				log.Info("AuthPolicy opted out, skipping", "name", authPolicyName)
			} else {
				// Snapshot the existing object before modifications so we can detect
				// no-op updates.
				snapshot := existing.DeepCopy()

				mergedAnnotations := existing.GetAnnotations()
				if mergedAnnotations == nil {
					mergedAnnotations = make(map[string]string)
				}
				for k, v := range authPolicy.GetAnnotations() {
					mergedAnnotations[k] = v
				}
				existing.SetAnnotations(mergedAnnotations)

				mergedLabels := existing.GetLabels()
				if mergedLabels == nil {
					mergedLabels = make(map[string]string)
				}
				for k, v := range authPolicy.GetLabels() {
					mergedLabels[k] = v
				}
				existing.SetLabels(mergedLabels)
				if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
					return nil, fmt.Errorf("failed to update spec: %w", err)
				}

				if equality.Semantic.DeepEqual(snapshot.Object, existing.Object) {
					log.Info("AuthPolicy unchanged, skipping update", "name", authPolicyName, "model", ref.Namespace+"/"+ref.Name)
				} else {
					if err := r.Update(ctx, existing); err != nil {
						return nil, fmt.Errorf("failed to update AuthPolicy for model %s/%s: %w", ref.Namespace, ref.Name, err)
					}
					log.Info("AuthPolicy updated", "name", authPolicyName, "model", ref.Namespace+"/"+ref.Name, "policies", policyNames)
				}
			}
		}
	}
	if err := r.cleanupStaleAuthPolicies(ctx, log, policy); err != nil {
		return nil, err
	}

	return refs, nil
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
		modelKey := ap.GetNamespace() + "/" + modelName
		if currentModels[modelKey] {
			continue
		}
		if !slices.Contains(strings.Split(ap.GetAnnotations()["maas.opendatahub.io/auth-policies"], ","), policy.Name) {
			continue
		}
		log.Info("Cleaning up stale AuthPolicy for removed modelRef", "model", modelKey, "authPolicy", ap.GetName())
		if err := r.deleteModelAuthPolicy(ctx, log, ap.GetNamespace(), modelName); err != nil {
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
			log.Info("Deleting model AuthPolicy so remaining policies can rebuild it", "model", ref.Namespace+"/"+ref.Name)
			if err := r.deleteModelAuthPolicy(ctx, log, ref.Namespace, ref.Name); err != nil {
				log.Error(err, "failed to clean up AuthPolicy, will retry", "model", ref.Namespace+"/"+ref.Name)
				return ctrl.Result{}, err
			}
		}
		// Also clean up stale AuthPolicies from modelRefs that were removed
		// before the CR was deleted (edge case: edit + delete before reconcile).
		if err := r.cleanupStaleAuthPolicies(ctx, log, policy); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *MaaSAuthPolicyReconciler) updateAuthPolicyRefStatus(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy, refs []authPolicyRef) {
	policy.Status.AuthPolicies = make([]maasv1alpha1.AuthPolicyRefStatus, 0, len(refs))
	for _, ref := range refs {
		ap := &unstructured.Unstructured{}
		ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		ap.SetNamespace(ref.Namespace)
		ap.SetName(ref.Name)
		if err := r.Get(ctx, client.ObjectKeyFromObject(ap), ap); err != nil {
			log.Info("could not get AuthPolicy for status", "name", ref.Name, "namespace", ref.Namespace, "error", err)
			policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, maasv1alpha1.AuthPolicyRefStatus{
				Name: ref.Name, Namespace: ref.Namespace, Model: ref.Model, ModelNamespace: ref.ModelNamespace, Accepted: "Unknown", Enforced: "Unknown",
			})
			continue
		}
		accepted, enforced := getAuthPolicyConditionState(ap)
		policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, maasv1alpha1.AuthPolicyRefStatus{
			Name: ref.Name, Namespace: ref.Namespace, Model: ref.Model, ModelNamespace: ref.ModelNamespace, Accepted: accepted, Enforced: enforced,
		})
	}
}

func getAuthPolicyConditionState(ap *unstructured.Unstructured) (accepted, enforced string) {
	accepted, enforced = "Unknown", "Unknown"
	conditions, found, err := unstructured.NestedSlice(ap.Object, "status", "conditions")
	if err != nil || !found || len(conditions) == 0 {
		return accepted, enforced
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		switch typ {
		case "Accepted":
			accepted = status
		case "Enforced":
			enforced = status
		}
	}
	return accepted, enforced
}

func (r *MaaSAuthPolicyReconciler) updateStatus(ctx context.Context, policy *maasv1alpha1.MaaSAuthPolicy, phase, message string, statusSnapshot *maasv1alpha1.MaaSAuthPolicyStatus) {
	policy.Status.Phase = phase

	status := metav1.ConditionTrue
	reason := "Reconciled"
	if phase == "Failed" {
		status = metav1.ConditionFalse
		reason = "ReconcileFailed"
	}

	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
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

	return ctrl.NewControllerManagedBy(mgr).
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
		Complete(r)
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
	modelNamespace := obj.GetNamespace()
	ap := findAnyAuthPolicyForModel(ctx, r.Client, modelNamespace, modelName)
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
