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

// reconcileGatewayAuthPolicy creates or updates the Gateway-level AuthPolicy for this tenant's gateway.
// Each tenant has its own Gateway (user-created) and needs its own AuthPolicy with the correct tenant identifier.
func (r *TenantReconciler) reconcileGatewayAuthPolicy(ctx context.Context, tenant *maasv1alpha1.Tenant, tenantID, appNamespace string) error {
	log := ctrl.LoggerFrom(ctx)

	gatewayNS := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name

	// Derive AuthPolicy name from gateway name to avoid collisions
	authPolicyName := fmt.Sprintf("%s-maas-auth", gatewayName)

	log.Info("Reconciling gateway AuthPolicy",
		"gatewayNamespace", gatewayNS,
		"gatewayName", gatewayName,
		"authPolicyName", authPolicyName,
		"tenantID", tenantID,
		"appNamespace", appNamespace)

	// Build the AuthPolicy spec
	spec := r.buildGatewayAuthPolicySpec(tenantID, appNamespace, gatewayNS, gatewayName)

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

// buildGatewayAuthPolicySpec builds the AuthPolicy spec for a tenant's gateway.
// This is similar to MaaSAuthPolicyReconciler.buildGatewayAuthPolicySpec but scoped to one tenant.
func (r *TenantReconciler) buildGatewayAuthPolicySpec(tenantID, appNamespace, gatewayNS, gatewayName string) map[string]any {
	// API key validation URL points to the per-tenant maas-api service
	// Default tenant (models-as-a-service): maas-api
	// AITenant-managed tenants: maas-api-{tenantID}
	serviceName := "maas-api"
	if tenantID != "" && tenantID != "models-as-a-service" {
		serviceName = fmt.Sprintf("maas-api-%s", tenantID)
	}
	apiKeyValidationURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/internal/v1/api-keys/validate", serviceName, appNamespace)

	return map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  gatewayName,
		},
		"rules": map[string]any{
			"authentication": map[string]any{
				// API key authentication
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
				// OpenShift/K8s token authentication
				"openshift-identities": map[string]any{
					"when": []any{
						map[string]any{
							"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
						},
					},
					"kubernetesTokenReview": map[string]any{
						"audiences": []any{r.ClusterAudience},
					},
					"priority": int64(1),
				},
			},
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
						"ttl": int64(60),
					},
					"metrics":  false,
					"priority": int64(0),
				},
			},
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
								"expression": `size(auth.identity.user.groups) > 0 ? '["' + auth.identity.user.groups.join('","') + '"]' : '[]'`,
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
