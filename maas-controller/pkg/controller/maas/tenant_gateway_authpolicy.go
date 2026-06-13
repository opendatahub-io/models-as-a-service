/*
Copyright 2026.

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
	"strconv"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	tenantGatewayAuthPolicyFinalizer = "maas.opendatahub.io/tenant-gateway-authpolicy"
)

// fetchOIDCConfigFromTenant fetches OIDC configuration from a Tenant CR.
// Returns nil if the Tenant doesn't have externalOIDC configured.
// TODO: Consolidate with maasauthpolicy_controller.go fetchOIDCConfig - these are duplicates.
func (r *TenantReconciler) fetchOIDCConfigFromTenant(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) *oidcConfig {
	// Extract spec.externalOIDC if present
	if tenant.Spec.ExternalOIDC == nil {
		log.V(1).Info("Tenant CR has no externalOIDC configuration")
		return nil
	}

	issuerURL := tenant.Spec.ExternalOIDC.IssuerURL
	clientID := tenant.Spec.ExternalOIDC.ClientID

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

// reconcileGatewayAuthPolicy creates or updates the Gateway-level AuthPolicy for this tenant's gateway.
// Each tenant has its own Gateway (user-created) and needs its own AuthPolicy with the correct tenant identifier.
func (r *TenantReconciler) reconcileGatewayAuthPolicy(ctx context.Context, tenant *maasv1alpha1.Tenant, tenantID, appNamespace string) error {
	log := ctrl.LoggerFrom(ctx)

	gatewayNS := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name

	// Use a well-known name for the default tenant's AuthPolicy for easier testing/debugging
	// Other tenants derive the name from their gateway name to avoid collisions
	authPolicyName := "maas-gateway-auth"
	if tenantID != "models-as-a-service" {
		authPolicyName = fmt.Sprintf("%s-maas-auth", gatewayName)
	}

	log.Info("Reconciling gateway AuthPolicy",
		"gatewayNamespace", gatewayNS,
		"gatewayName", gatewayName,
		"authPolicyName", authPolicyName,
		"tenantID", tenantID,
		"appNamespace", appNamespace)

	// Fetch OIDC configuration if present
	oidc := r.fetchOIDCConfigFromTenant(ctx, log, tenant)

	// Build the AuthPolicy spec
	spec := r.buildGatewayAuthPolicySpec(tenantID, appNamespace, gatewayNS, gatewayName, oidc)

	// Create the AuthPolicy resource
	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kuadrant.io",
		Version: "v1",
		Kind:    "AuthPolicy",
	})
	authPolicy.SetName(authPolicyName)
	authPolicy.SetNamespace(gatewayNS)
	authPolicy.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-tenant-gateway-auth",
		"app.kubernetes.io/component":  "gateway-auth",
		tenantreconcile.LabelTenantName: tenant.Name,
		tenantreconcile.LabelTenantNamespace: tenant.Namespace,
	})

	// Add finalizer to Tenant CR for cleanup
	if !controllerutil.ContainsFinalizer(tenant, tenantGatewayAuthPolicyFinalizer) {
		controllerutil.AddFinalizer(tenant, tenantGatewayAuthPolicyFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	// Check if AuthPolicy already exists
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
	err := r.Get(ctx, client.ObjectKeyFromObject(authPolicy), existing)

	if apierrors.IsNotFound(err) {
		// Create new AuthPolicy
		if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to set AuthPolicy spec: %w", err)
		}
		if err := r.Create(ctx, authPolicy); err != nil {
			return fmt.Errorf("failed to create gateway AuthPolicy: %w", err)
		}
		log.Info("Created gateway AuthPolicy", "name", authPolicyName, "namespace", gatewayNS)
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to get gateway AuthPolicy: %w", err)
	}

	// Update existing AuthPolicy
	if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set AuthPolicy spec for update: %w", err)
	}
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update gateway AuthPolicy: %w", err)
	}
	log.Info("Updated gateway AuthPolicy", "name", authPolicyName, "namespace", gatewayNS)
	return nil
}

// buildAuthenticationSection builds the authentication section of an AuthPolicy.
// Includes API keys (priority 0), OIDC if configured (priority 1), and K8s tokens (priority 2).
// TODO: Consolidate with MaaSAuthPolicyReconciler authentication building logic.
func (r *TenantReconciler) buildAuthenticationSection(oidc *oidcConfig) map[string]any {
	auth := map[string]any{
		// API key authentication - highest priority
		"api-keys": map[string]any{
			"when": []any{
				map[string]any{
					"selector": "request.headers.authorization",
					"operator": "matches",
					"value":    "^Bearer sk-oai-.*",
				},
			},
			"credentials": map[string]any{
				"authorizationHeader": map[string]any{
					"prefix": "Bearer",
				},
			},
			"plain": map[string]any{
				"selector": "request.headers.authorization",
			},
			"metrics":  false,
			"priority": int64(0),
		},
	}

	// Add OIDC authentication if configured
	if oidc != nil {
		auth["oidc-identities"] = map[string]any{
			"when": []any{
				map[string]any{
					// Match JWT-shaped tokens only (three dot-separated segments: header.payload.signature)
					// This prevents service account tokens from hitting JWT validation
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
				},
			},
			"jwt": map[string]any{
				"issuerUrl": oidc.IssuerURL,
			},
			"credentials": map[string]any{
				"authorizationHeader": map[string]any{
					"prefix": "Bearer",
				},
			},
			"metrics":  false,
			"priority": int64(1),
		}
		// K8s tokens have lower priority when OIDC is present
		auth["openshift-identities"] = map[string]any{
			"when": []any{
				map[string]any{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
				},
			},
			"kubernetesTokenReview": map[string]any{
				"audiences": []any{r.ClusterAudience},
			},
			"priority": int64(2),
		}
	} else {
		// No OIDC - K8s tokens are priority 1
		auth["openshift-identities"] = map[string]any{
			"when": []any{
				map[string]any{
					"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
				},
			},
			"kubernetesTokenReview": map[string]any{
				"audiences": []any{r.ClusterAudience},
			},
			"priority": int64(1),
		}
	}

	return auth
}

// buildGatewayAuthPolicySpec builds the AuthPolicy spec for a tenant's gateway.
// This is similar to MaaSAuthPolicyReconciler.buildGatewayAuthPolicySpec but scoped to one tenant.
// TODO: Consolidate gateway AuthPolicy building logic between TenantReconciler and MaaSAuthPolicyReconciler.
func (r *TenantReconciler) buildGatewayAuthPolicySpec(tenantID, appNamespace, gatewayNS, gatewayName string, oidc *oidcConfig) map[string]any {
	// API key validation URL points to the per-tenant maas-api service
	// Use the same naming convention as MaaSAPIServiceName from tenantreconcile
	serviceName := tenantreconcile.MaaSAPIServiceName(tenantID)
	apiKeyValidationURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/internal/v1/api-keys/validate", serviceName, appNamespace)
	subscriptionSelectorURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/internal/v1/subscriptions/select", serviceName, appNamespace)

	return map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  gatewayName,
		},
		"rules": map[string]any{
			"authentication": r.buildAuthenticationSection(oidc),
			"metadata": map[string]any{
				// API key validation
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
				// Subscription selection for rate limiting
				"subscription-info": r.buildSubscriptionInfoMetadata(subscriptionSelectorURL, tenantID),
			},
			"authorization": r.buildAuthorizationSection(tenantID),
			"response": map[string]any{
				"success": map[string]any{
					"headers": map[string]any{
						// Username headers
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
							"priority": int64(1),
						},
						// Group headers
						"X-MaaS-Group": map[string]any{
							"when": []any{
								map[string]any{
									"selector": "request.headers.authorization",
									"operator": "matches",
									"value":    "^Bearer sk-oai-.*",
								},
							},
							"plain": map[string]any{
								"expression": `size(auth.metadata.apiKeyValidation.groups) > 0 ? '["' + auth.metadata.apiKeyValidation.groups.join('","') + '"]' : '[]'`,
							},
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
							"priority": int64(1),
						},
						// Tenant headers - CRITICAL for multi-tenancy
						"X-MaaS-Tenant": map[string]any{
							"when": []any{
								map[string]any{
									"selector": "request.headers.authorization",
									"operator": "matches",
									"value":    "^Bearer sk-oai-.*",
								},
							},
							"plain": map[string]any{
								"selector": "auth.metadata.apiKeyValidation.tenant",
							},
							"priority": int64(0),
						},
						"X-MaaS-Tenant-Token": map[string]any{
							"when": []any{
								map[string]any{
									"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
								},
							},
							"plain": map[string]any{
								// Use the tenant identifier for this specific tenant
								// For default tenant: "" (empty string)
								// For AITenant: "redteam", "blueteam", etc.
								"expression": strconv.Quote(tenantID),
							},
							"key":      "X-MaaS-Tenant",
							"priority": int64(1),
						},
						// Subscription header
						"X-MaaS-Subscription": map[string]any{
							"when": []any{
								map[string]any{
									"selector": "auth.metadata.apiKeyValidation.subscription",
									"operator": "neq",
									"value":    "",
								},
							},
							"plain": map[string]any{
								"selector": "auth.metadata.apiKeyValidation.subscription",
							},
						},
					},
					// Identity filters for rate limiting
					"filters": map[string]any{
						"identity": r.buildIdentityFilters(tenantID),
					},
				},
			},
		},
	}
}

// cleanupGatewayAuthPolicy removes the gateway AuthPolicy when the Tenant is deleted.
func (r *TenantReconciler) cleanupGatewayAuthPolicy(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) error {
	if !controllerutil.ContainsFinalizer(tenant, tenantGatewayAuthPolicyFinalizer) {
		return nil
	}

	gatewayNS := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name
	authPolicyName := fmt.Sprintf("%s-maas-auth", gatewayName)

	log.Info("Cleaning up gateway AuthPolicy", "name", authPolicyName, "namespace", gatewayNS)

	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kuadrant.io",
		Version: "v1",
		Kind:    "AuthPolicy",
	})
	authPolicy.SetName(authPolicyName)
	authPolicy.SetNamespace(gatewayNS)

	if err := r.Delete(ctx, authPolicy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete gateway AuthPolicy: %w", err)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(tenant, tenantGatewayAuthPolicyFinalizer)
	if err := r.Update(ctx, tenant); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	log.Info("Gateway AuthPolicy cleanup complete")
	return nil
}

// buildSubscriptionInfoMetadata builds the subscription-info metadata evaluator for rate limiting.
// This makes an HTTP call to the maas-api subscription selector to determine which subscription
// the user should use for the requested model.
func (r *TenantReconciler) buildSubscriptionInfoMetadata(subscriptionSelectorURL, tenantID string) map[string]any {
	// CEL expressions for extracting model identity from path or header
	// For /llm/facebook-opt-125m-simulated, this returns "llm/facebook-opt-125m-simulated"
	celModelIdentity := `(request.path.startsWith("/llm/") ? request.path.split("/").filter(x, x != "")[0] + "/" + request.path.split("/").filter(x, x != "")[1] : ("x-gateway-model-name" in request.headers ? request.headers["x-gateway-model-name"] : ""))`

	// Username extraction
	celUsername := `has(auth.metadata.apiKeyValidation.username) ? auth.metadata.apiKeyValidation.username : (has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// Groups extraction
	celGroups := `has(auth.metadata.apiKeyValidation.groups) ? auth.metadata.apiKeyValidation.groups : (has(auth.identity.groups) ? auth.identity.groups : auth.identity.user.groups)`

	// Subscription field from API key validation or header
	celSubscription := `has(auth.metadata.apiKeyValidation.subscription) && auth.metadata.apiKeyValidation.subscription != "" ? auth.metadata.apiKeyValidation.subscription : (has(request.headers["x-maas-subscription"]) ? request.headers["x-maas-subscription"] : "")`

	// Tenant identifier for this gateway
	celTenantID := strconv.Quote(tenantID)

	// Request body for subscription selector
	subscriptionInfoBody := fmt.Sprintf(
		`{"userId": %s, "groups": %s, "subscription": %s, "requestedModel": %s, "tenant": %s}`,
		celUsername, celGroups, celSubscription, celModelIdentity, celTenantID)

	// Cache key for subscription selector (user + groups + subscription + model)
	cacheKeySelector := fmt.Sprintf(`%s + "|" + (%s).join(",") + "|" + %s + "|" + %s`,
		celUsername, celGroups, celSubscription, celModelIdentity)

	return map[string]any{
		"when": []any{
			map[string]any{
				"predicate": `request.path.startsWith("/llm/") || "x-gateway-model-name" in request.headers`,
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
				"selector": cacheKeySelector,
			},
			"ttl": r.MetadataCacheTTL,
		},
		"metrics":  false,
		"priority": int64(1),
	}
}

// buildAuthorizationSection builds the authorization rules for the gateway AuthPolicy.
// Includes validation for API keys, subscriptions, and group-based model access control.
func (r *TenantReconciler) buildAuthorizationSection(tenantID string) map[string]any {
	// Auth-valid cache key
	authValidCacheKey := `has(auth.metadata.apiKeyValidation.username) ? auth.metadata.apiKeyValidation.username : (has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// Model identity CEL for cache key
	celModelIdentity := `request.path.startsWith("/llm/") ? request.path.split("/")[2] : request.headers["x-gateway-model-name"]`

	// Username extraction
	celUsername := `has(auth.metadata.apiKeyValidation.username) ? auth.metadata.apiKeyValidation.username : (has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// Groups extraction
	celGroups := `has(auth.metadata.apiKeyValidation.groups) ? auth.metadata.apiKeyValidation.groups : (has(auth.identity.groups) ? auth.identity.groups : auth.identity.user.groups)`

	// Subscription cache key (user + groups + subscription + model)
	celSubscription := `has(auth.metadata.apiKeyValidation.subscription) && auth.metadata.apiKeyValidation.subscription != "" ? auth.metadata.apiKeyValidation.subscription : (has(request.headers["x-maas-subscription"]) ? request.headers["x-maas-subscription"] : "")`
	subscriptionCacheKey := fmt.Sprintf(`%s + "|" + (%s).join(",") + "|" + %s + "|" + %s`,
		celUsername, celGroups, celSubscription, celModelIdentity)

	return map[string]any{
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
				"ttl": r.MetadataCacheTTL,
			},
		},
		"subscription-valid": map[string]any{
			"when": []any{
				map[string]any{
					"predicate": `request.path.startsWith("/llm/") || "x-gateway-model-name" in request.headers`,
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
					"selector": subscriptionCacheKey,
				},
				"ttl": r.MetadataCacheTTL,
			},
		},
	}
}

// buildIdentityFilters builds the identity filters for rate limiting.
// These filters extract user identity, groups, and subscription information from auth metadata.
func (r *TenantReconciler) buildIdentityFilters(tenantID string) map[string]any {
	// Username extraction
	celUsername := `has(auth.metadata.apiKeyValidation.username) ? auth.metadata.apiKeyValidation.username : (has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username))`

	// Groups extraction
	celGroups := `has(auth.metadata.apiKeyValidation.groups) ? auth.metadata.apiKeyValidation.groups : (has(auth.identity.groups) ? auth.identity.groups : auth.identity.user.groups)`

	// Model identity from path or header - includes namespace/model-name for /llm/ paths
	// For /llm/facebook-opt-125m-simulated, this returns "llm/facebook-opt-125m-simulated"
	celModelIdentity := `(request.path.startsWith("/llm/") ? request.path.split("/").filter(x, x != "")[0] + "/" + request.path.split("/").filter(x, x != "")[1] : ("x-gateway-model-name" in request.headers ? request.headers["x-gateway-model-name"] : ""))`

	return map[string]any{
		"json": map[string]any{
			"properties": map[string]any{
				"groups": map[string]any{
					"expression": celGroups,
				},
				"groups_str": map[string]any{
					"expression": fmt.Sprintf(`(%s).join(",")`, celGroups),
				},
				"userid": map[string]any{
					"expression": celUsername,
				},
				"keyId": map[string]any{
					"expression": `has(auth.metadata.apiKeyValidation) ? auth.metadata.apiKeyValidation.keyId : ""`,
				},
				"selected_subscription": map[string]any{
					"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"].name : ""`,
				},
				// Model-scoped subscription key: namespace/name@modelIdentity
				// This is what TokenRateLimitPolicy uses for rate limiting
				"selected_subscription_key": map[string]any{
					"expression": fmt.Sprintf(
						`has(auth.metadata["subscription-info"].namespace) && `+
							`has(auth.metadata["subscription-info"].name) `+
							`? auth.metadata["subscription-info"].namespace + "/" `+
							`+ auth.metadata["subscription-info"].name + "@" + %s : ""`,
						celModelIdentity),
				},
				"selected_subscription_obj": map[string]any{
					"expression": `has(auth.metadata["subscription-info"].name) ? auth.metadata["subscription-info"] : {}`,
				},
				"subscription_error": map[string]any{
					"expression": `has(auth.metadata["subscription-info"].error) ? auth.metadata["subscription-info"].error : ""`,
				},
			},
		},
		"metrics":  true,
		"priority": int64(0),
	}
}
