package tenantreconcile

import (
	"fmt"
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

func TestPatchMaaSAPIDeploymentReplicas(t *testing.T) {
	makeDeployment := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"spec": map[string]any{
					"replicas": int64(1),
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "maas-api",
									"image": "quay.io/opendatahub/maas-api:latest",
									"env":   []any{},
								},
							},
						},
					},
				},
			},
		}
	}

	t.Run("nil replicas leaves default unchanged", func(t *testing.T) {
		deployment := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			APIKeyMaxExpirationDays: "90",
			MaaSAPIReplicas:         nil,
		}

		err := patchMaaSAPIDeployment(logr.Discard(), deployment, params)
		require.NoError(t, err)

		replicas, found, err := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, int64(1), replicas)
	})

	t.Run("non-nil replicas overrides default", func(t *testing.T) {
		deployment := makeDeployment()
		r := int32(3)
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			APIKeyMaxExpirationDays: "90",
			MaaSAPIReplicas:         &r,
		}

		err := patchMaaSAPIDeployment(logr.Discard(), deployment, params)
		require.NoError(t, err)

		replicas, found, err := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, int64(3), replicas)
	})
}

func TestPatchPayloadProcessingDeploymentReplicas(t *testing.T) {
	makeDeployment := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name":      "payload-processing",
					"namespace": "opendatahub",
				},
				"spec": map[string]any{
					"replicas": int64(1),
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "payload-processing",
									"image": "quay.io/opendatahub/payload-processing:latest",
								},
							},
						},
					},
				},
			},
		}
	}

	t.Run("nil replicas leaves default unchanged", func(t *testing.T) {
		deployment := makeDeployment()
		params := PlatformParams{
			GatewayNamespace:          "gateway-ns",
			PayloadProcessingImage:    "test-image",
			PayloadProcessingReplicas: nil,
		}

		err := patchPayloadProcessingDeployment(logr.Discard(), deployment, params)
		require.NoError(t, err)

		replicas, found, err := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, int64(1), replicas)
	})

	t.Run("non-nil replicas overrides default", func(t *testing.T) {
		deployment := makeDeployment()
		r := int32(2)
		params := PlatformParams{
			GatewayNamespace:          "gateway-ns",
			PayloadProcessingImage:    "test-image",
			PayloadProcessingReplicas: &r,
		}

		err := patchPayloadProcessingDeployment(logr.Discard(), deployment, params)
		require.NoError(t, err)

		replicas, found, err := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, int64(2), replicas)
	})
}

func TestPatchMaaSAPIDeploymentTENANT_NAME(t *testing.T) {
	tests := []struct {
		name               string
		tenantID           string
		expectedTenantName string
	}{
		{
			name:               "default tenant gets models-as-a-service TENANT_NAME",
			tenantID:           "",
			expectedTenantName: "models-as-a-service",
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

func TestPatchPayloadProcessingTracing(t *testing.T) {
	deployment := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{
								"name": "payload-processing",
								"args": []any{"--tracing=false"},
								"env": []any{
									map[string]any{
										"name":  "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
										"value": "http://data-science-collector-collector.opendatahub.svc:4317",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	params := PlatformParams{MonitoringNamespace: "redhat-ods-monitoring"}
	err := patchPayloadProcessingTracing(logr.Discard(), deployment, params)
	require.NoError(t, err)

	assertContainerArg(t, deployment, "payload-processing", "--tracing=true")
	wantEndpoint := "http://data-science-collector-collector.redhat-ods-monitoring.svc:4317"
	assert.Equal(t, wantEndpoint, requireEnvVarValue(t, deployment, "payload-processing", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	assert.Equal(t, wantEndpoint, requireEnvVarValue(t, deployment, "payload-processing", "OTEL_EXPORTER_OTLP_ENDPOINT"))
}

func TestPatchPayloadProcessingTracingMonitoringDisabled(t *testing.T) {
	deployment := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{
								"name": "payload-processing",
								"args": []any{"--tracing=true"},
							},
						},
					},
				},
			},
		},
	}

	err := patchPayloadProcessingTracing(logr.Discard(), deployment, PlatformParams{})
	require.NoError(t, err)
	assertContainerArg(t, deployment, "payload-processing", "--tracing=false")
}

func TestPatchNetworkPolicyOTLPEgress(t *testing.T) {
	np := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"spec": map[string]any{
				"egress": []any{
					map[string]any{
						"ports": []any{
							map[string]any{"port": int64(4317), "protocol": "TCP"},
						},
						"to": []any{
							map[string]any{
								"namespaceSelector": map[string]any{
									"matchLabels": map[string]any{
										"kubernetes.io/metadata.name": "opendatahub",
									},
								},
								"podSelector": map[string]any{
									"matchLabels": map[string]any{
										DefaultOTLPCollectorPodLabelKey:       DefaultOTLPCollectorService,
										DefaultOTLPCollectorComponentLabelKey: DefaultOTLPCollectorComponentLabelValue,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := patchNetworkPolicyOTLPEgress(np, "redhat-ods-monitoring")
	require.NoError(t, err)

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	rule, ok := egress[0].(map[string]any)
	require.True(t, ok)
	to, ok := rule["to"].([]any)
	require.True(t, ok)
	peer, ok := to[0].(map[string]any)
	require.True(t, ok)
	nsSelector, ok := peer["namespaceSelector"].(map[string]any)
	require.True(t, ok)
	matchLabels, ok := nsSelector["matchLabels"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "redhat-ods-monitoring", matchLabels["kubernetes.io/metadata.name"])

	podSelector, ok := peer["podSelector"].(map[string]any)
	require.True(t, ok)
	podMatchLabels, ok := podSelector["matchLabels"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, DefaultOTLPCollectorService, podMatchLabels[DefaultOTLPCollectorPodLabelKey])
	assert.Equal(t, DefaultOTLPCollectorComponentLabelValue, podMatchLabels[DefaultOTLPCollectorComponentLabelKey])
}

func TestPatchNetworkPolicyOTLPEgressRewritesKustomizePollutedPodSelector(t *testing.T) {
	np := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"spec": map[string]any{
				"egress": []any{
					map[string]any{
						"ports": []any{
							map[string]any{"port": int64(4317), "protocol": "TCP"},
						},
						"to": []any{
							map[string]any{
								"namespaceSelector": map[string]any{
									"matchLabels": map[string]any{
										"kubernetes.io/metadata.name": "opendatahub",
									},
								},
								"podSelector": map[string]any{
									"matchLabels": map[string]any{
										"app.kubernetes.io/name":      "payload-processing",
										"app.kubernetes.io/component": "payload-processing",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := patchNetworkPolicyOTLPEgress(np, "redhat-ods-monitoring")
	require.NoError(t, err)

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	rule, ok := egress[0].(map[string]any)
	require.True(t, ok)
	to, ok := rule["to"].([]any)
	require.True(t, ok)
	peer, ok := to[0].(map[string]any)
	require.True(t, ok)
	podSelector, ok := peer["podSelector"].(map[string]any)
	require.True(t, ok)
	podMatchLabels, ok := podSelector["matchLabels"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, DefaultOTLPCollectorService, podMatchLabels[DefaultOTLPCollectorPodLabelKey])
	assert.Equal(t, DefaultOTLPCollectorComponentLabelValue, podMatchLabels[DefaultOTLPCollectorComponentLabelKey])
}

func TestPatchNetworkPolicyOTLPEgressMissingRule(t *testing.T) {
	np := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"spec": map[string]any{
				"egress": []any{
					map[string]any{
						"ports": []any{
							map[string]any{"port": int64(443), "protocol": "TCP"},
						},
					},
				},
			},
		},
	}

	err := patchNetworkPolicyOTLPEgress(np, "redhat-ods-monitoring")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing OTLP egress rule")
}

func TestPatchPayloadProcessingEnvoyFilterOmitsRouterFallback(t *testing.T) {
	resources := renderOverlayResources(t, "tenant-ns")
	ef := requireResource(t, resources, GVKEnvoyFilter, PayloadProcessingEnvoyFilterName(""))
	params := PlatformParams{
		GatewayNamespace:                       "gateway-ns",
		GatewayName:                            "custom-gateway",
		AppNamespace:                           "tenant-ns",
		PayloadProcessingRouterExtProcFallback: false,
	}

	err := patchPayloadProcessingEnvoyFilter(logr.Discard(), ef, params)
	require.NoError(t, err)

	configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, configPatches, 11)

	for i := 4; i < 9; i++ {
		cp, ok := configPatches[i].(map[string]any)
		require.True(t, ok)
		op, _, _ := unstructured.NestedString(cp, "patch", "operation")
		assert.Equal(t, "MERGE", op)
	}
	requireEPPReorderPatches(t, configPatches)
}

func TestPatchPayloadProcessingEnvoyFilterKeepsRouterFallback(t *testing.T) {
	resources := renderOverlayResources(t, "tenant-ns")
	ef := requireResource(t, resources, GVKEnvoyFilter, PayloadProcessingEnvoyFilterName(""))
	params := PlatformParams{
		GatewayNamespace:                       "gateway-ns",
		GatewayName:                            "custom-gateway",
		AppNamespace:                           "tenant-ns",
		PayloadProcessingRouterExtProcFallback: true,
	}

	err := patchPayloadProcessingEnvoyFilter(logr.Discard(), ef, params)
	require.NoError(t, err)

	configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, configPatches, 9)

	wantClusters := []string{
		grpcClusterName(PayloadPreProcessingDeploymentName(""), params.GatewayNamespace, 9004),
		grpcClusterName(PayloadProcessingDeploymentName(""), params.GatewayNamespace, 9004),
	}
	for i, wantCluster := range wantClusters {
		cp, ok := configPatches[i].(map[string]any)
		require.True(t, ok)
		anchor, _, _ := unstructured.NestedString(cp, "match", "listener", "filterChain", "filter", "subFilter", "name")
		assert.Equal(t, routerFilterName, anchor, "configPatches[%d] subFilter.name", i)
		cluster, _, _ := unstructured.NestedString(cp, "patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name")
		assert.Equal(t, wantCluster, cluster, "configPatches[%d] grpc cluster_name", i)
	}
	for i := 2; i < 7; i++ {
		cp, ok := configPatches[i].(map[string]any)
		require.True(t, ok)
		routeName, _, _ := unstructured.NestedString(cp, "match", "routeConfiguration", "vhost", "route", "name")
		assert.Equal(t, fmt.Sprintf("tenant-ns.%s.%d", MaaSAPIRouteName(""), i-2), routeName, "configPatches[%d] route name", i)
	}
	requireEPPReorderPatches(t, configPatches)
}

// requireEPPReorderPatches asserts that configPatches ends with the pair moving Istio's
// InferencePool filter in front of the router: REMOVE, then INSERT_BEFORE the router with
// Istio's static filter config, both listed after every ipp insert (router-anchored in the
// fallback, so list order decides) and untouched by the route rewrites.
func requireEPPReorderPatches(t *testing.T, configPatches []any) {
	t.Helper()
	require.GreaterOrEqual(t, len(configPatches), 2)
	removeIdx := len(configPatches) - 2
	subFilterPath := []string{"match", "listener", "filterChain", "filter", "subFilter", "name"}

	wantOps := []string{"REMOVE", "INSERT_BEFORE"}
	wantAnchors := []string{"envoy.filters.http.ext_proc", routerFilterName}

	for i, raw := range configPatches[:removeIdx] {
		cp, ok := raw.(map[string]any)
		require.True(t, ok, "configPatches[%d] should be a map", i)
		name, _, _ := unstructured.NestedString(cp, "patch", "value", "name")
		assert.NotEqual(t, "envoy.filters.http.ext_proc", name, "configPatches[%d] inserts the EPP filter before the reorder", i)
	}
	for j, raw := range configPatches[removeIdx:] {
		i := removeIdx + j
		cp, ok := raw.(map[string]any)
		require.True(t, ok, "configPatches[%d] should be a map", i)

		applyTo, _, _ := unstructured.NestedString(cp, "applyTo")
		assert.Equal(t, "HTTP_FILTER", applyTo, "configPatches[%d] applyTo", i)
		op, _, _ := unstructured.NestedString(cp, "patch", "operation")
		assert.Equal(t, wantOps[j], op, "configPatches[%d] operation", i)
		anchor, _, _ := unstructured.NestedString(cp, subFilterPath...)
		assert.Equal(t, wantAnchors[j], anchor, "configPatches[%d] subFilter.name", i)
		_, found, err := unstructured.NestedFieldNoCopy(cp, "match", "routeConfiguration")
		require.NoError(t, err)
		assert.False(t, found, "configPatches[%d] must not carry a route match", i)
	}

	insert, ok := configPatches[removeIdx+1].(map[string]any)
	require.True(t, ok)
	name, _, _ := unstructured.NestedString(insert, "patch", "value", "name")
	assert.Equal(t, "envoy.filters.http.ext_proc", name, "per-route EPP overrides are keyed on this name")
	typedConfig, found, err := unstructured.NestedMap(insert, "patch", "value", "typed_config")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, map[string]any{
		"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
		"grpc_service": map[string]any{
			"envoy_grpc": map[string]any{"cluster_name": "dummy"},
			"timeout":    "10s",
		},
		"failure_mode_allow": true,
		"processing_mode": map[string]any{
			"request_header_mode":  "SKIP",
			"response_header_mode": "SKIP",
		},
		"message_timeout": "1000s",
		"metadata_options": map[string]any{
			"forwarding_namespaces": map[string]any{"untyped": []any{"envoy.lb"}},
			"receiving_namespaces":  map[string]any{"untyped": []any{"envoy.lb"}},
		},
	}, typedConfig, "typed_config must stay the copy of Istio's static InferencePool filter")
}
