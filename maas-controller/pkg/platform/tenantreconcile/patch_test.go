package tenantreconcile

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPatchHTTPRouteBackendRefs(t *testing.T) {
	tests := []struct {
		name                string
		tenantID            string
		expectedServiceName string
	}{
		{
			name:                "default tenant uses base service name",
			tenantID:            "",
			expectedServiceName: "maas-api",
		},
		{
			name:                "redteam tenant uses suffixed service name",
			tenantID:            "redteam",
			expectedServiceName: "maas-api-redteam",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create HTTPRoute with two rules (mimics real maas-api HTTPRoute structure)
			route := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "gateway.networking.k8s.io/v1",
					"kind":       "HTTPRoute",
					"metadata": map[string]any{
						"name":      "maas-api-route",
						"namespace": "opendatahub",
					},
					"spec": map[string]any{
						"parentRefs": []any{
							map[string]any{
								"name":      "maas-default-gateway",
								"namespace": "openshift-ingress",
							},
						},
						"rules": []any{
							// Rule 1: /v1/models endpoint
							map[string]any{
								"matches": []any{
									map[string]any{
										"path": map[string]any{
											"type":  "PathPrefix",
											"value": "/v1/models",
										},
									},
								},
								"backendRefs": []any{
									map[string]any{
										"name": "maas-api",
										"port": int64(8080),
									},
								},
							},
							// Rule 2: /maas-api endpoint
							map[string]any{
								"matches": []any{
									map[string]any{
										"path": map[string]any{
											"type":  "PathPrefix",
											"value": "/maas-api",
										},
									},
								},
								"backendRefs": []any{
									map[string]any{
										"name": "maas-api",
										"port": int64(8080),
									},
								},
							},
						},
					},
				},
			}

			params := PlatformParams{
				GatewayNamespace: "openshift-ingress",
				GatewayName:      "test-gateway",
				TenantIdentifier: tt.tenantID,
			}

			err := patchHTTPRoute(logr.Discard(), route, params)
			require.NoError(t, err)

			// Verify parentRefs were updated
			parentRefs, found, err := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, parentRefs, 1)
			parentRef, ok := parentRefs[0].(map[string]any)
			require.True(t, ok, "parentRef should be a map")
			assert.Equal(t, "test-gateway", parentRef["name"])
			assert.Equal(t, "openshift-ingress", parentRef["namespace"])

			// Verify backendRefs in all rules were updated to per-tenant Service
			rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, rules, 2)

			for i, ruleRaw := range rules {
				rule, ok := ruleRaw.(map[string]any)
				require.True(t, ok, "rule should be a map")
				backendRefs, found, err := unstructured.NestedSlice(rule, "backendRefs")
				require.NoError(t, err, "rule %d should have backendRefs", i)
				require.True(t, found)
				require.Len(t, backendRefs, 1)

				backendRef, ok := backendRefs[0].(map[string]any)
				require.True(t, ok, "backendRef should be a map")
				assert.Equal(t, tt.expectedServiceName, backendRef["name"],
					"rule %d backendRef should point to %s", i, tt.expectedServiceName)
				assert.Equal(t, int64(8080), backendRef["port"])
			}
		})
	}
}

func TestPatchMaaSAPIAuthPolicyTargetRef(t *testing.T) {
	tests := []struct {
		name               string
		tenantID           string
		expectedRouteName  string
		expectedServiceURL string
	}{
		{
			name:               "default tenant targets base HTTPRoute and Service",
			tenantID:           "",
			expectedRouteName:  "maas-api-route",
			expectedServiceURL: "https://maas-api.opendatahub.svc.cluster.local:8443/internal/v1/api-keys/validate",
		},
		{
			name:               "redteam tenant targets redteam HTTPRoute and Service",
			tenantID:           "redteam",
			expectedRouteName:  "maas-api-route-redteam",
			expectedServiceURL: "https://maas-api-redteam.redhat-ai-gateway-infra.svc.cluster.local:8443/internal/v1/api-keys/validate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authPolicy := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "kuadrant.io/v1",
					"kind":       "AuthPolicy",
					"metadata": map[string]any{
						"name": "maas-api-auth-policy",
					},
					"spec": map[string]any{
						"targetRef": map[string]any{
							"group": "gateway.networking.k8s.io",
							"kind":  "HTTPRoute",
							"name":  "maas-api-route",
						},
						"rules": map[string]any{
							"authentication": map[string]any{
								"openshift-identities": map[string]any{
									"kubernetesTokenReview": map[string]any{
										"audiences": []any{"https://kubernetes.default.svc"},
									},
								},
							},
							"metadata": map[string]any{
								"apiKeyValidation": map[string]any{
									"http": map[string]any{
										"url":    "https://maas-api.placehold.svc.cluster.local:8443/internal/v1/api-keys/validate",
										"method": "POST",
									},
								},
							},
						},
					},
				},
			}

			appNamespace := "opendatahub"
			if tt.tenantID != "" {
				appNamespace = DefaultMaaSAPINamespace
			}

			params := PlatformParams{
				AppNamespace:     appNamespace,
				ClusterAudience:  "https://kubernetes.default.svc",
				TenantIdentifier: tt.tenantID,
			}

			err := patchMaaSAPIAuthPolicy(logr.Discard(), authPolicy, params)
			require.NoError(t, err)

			// Verify targetRef points to per-tenant HTTPRoute
			targetRefName, found, err := unstructured.NestedString(authPolicy.Object, "spec", "targetRef", "name")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tt.expectedRouteName, targetRefName)

			// Verify validation URL points to per-tenant Service
			validationURL, found, err := unstructured.NestedString(authPolicy.Object,
				"spec", "rules", "metadata", "apiKeyValidation", "http", "url")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tt.expectedServiceURL, validationURL)
		})
	}
}

func TestPatchMaaSAPIDeploymentTENANT_NAME(t *testing.T) {
	tests := []struct {
		name               string
		tenantID           string
		expectedTenantName string
	}{
		{
			name:               "default tenant gets empty TENANT_NAME",
			tenantID:           "",
			expectedTenantName: "",
		},
		{
			name:               "redteam tenant gets TENANT_NAME=redteam",
			tenantID:           "redteam",
			expectedTenantName: "redteam",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"spec": map[string]any{
						"template": map[string]any{
							"spec": map[string]any{
								"containers": []any{
									map[string]any{
										"name":  "maas-api",
										"image": "quay.io/opendatahub/maas-api:latest",
										"env": []any{
											map[string]any{
												"name":  "SOME_OTHER_VAR",
												"value": "foo",
											},
										},
									},
								},
							},
						},
					},
				},
			}

			params := PlatformParams{
				TenantIdentifier:        tt.tenantID,
				GatewayNamespace:        "openshift-ingress",
				GatewayName:             "test-gateway",
				MaaSAPIImage:            "test-image",
				APIKeyMaxExpirationDays: "90",
			}

			err := patchMaaSAPIDeployment(logr.Discard(), deployment, params)
			require.NoError(t, err)

			// Verify TENANT_NAME env var was set
			containers, found, err := unstructured.NestedSlice(deployment.Object,
				"spec", "template", "spec", "containers")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, containers, 1)

			container, ok := containers[0].(map[string]any)
			require.True(t, ok, "container should be a map")
			envVars, ok := container["env"].([]any)
			require.True(t, ok, "env should be a slice")

			var tenantNameValue string
			var foundTenantName bool
			for _, envVar := range envVars {
				ev, ok := envVar.(map[string]any)
				require.True(t, ok, "env var should be a map")
				if ev["name"] == "TENANT_NAME" {
					tenantNameValue, ok = ev["value"].(string)
					require.True(t, ok, "TENANT_NAME value should be a string")
					foundTenantName = true
					break
				}
			}

			require.True(t, foundTenantName, "TENANT_NAME env var should be set")
			assert.Equal(t, tt.expectedTenantName, tenantNameValue)
		})
	}
}
