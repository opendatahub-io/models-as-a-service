package tenantreconcile

import (
	"context"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

func TestBuildTelemetryLabels(t *testing.T) {
	telemetryTenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "models-as-a-service",
			Labels: map[string]string{
				LabelTenantName:      "models-as-a-service",
				LabelTenantNamespace: "models-as-a-service",
			},
		},
	}
	modelLabel := staticTelemetryModelLabel(t)
	tests := []struct {
		name           string
		config         *maasv1alpha1.TenantTelemetryConfig
		expectedLabels map[string]any
		absentKeys     []string
	}{
		{
			name:   "nil config uses defaults",
			config: nil,
			expectedLabels: map[string]any{
				"tenant_name":      `"models-as-a-service"`,
				"tenant_namespace": `"models-as-a-service"`,
				"subscription":     "auth.identity.selected_subscription",
				"cost_center":      `has(auth.identity.subscription_info.costCenter) ? auth.identity.subscription_info.costCenter : ""`,
				"organization_id":  `has(auth.identity.subscription_info.organizationId) ? auth.identity.subscription_info.organizationId : ""`,
				"model":            modelLabel,
			},
			absentKeys: []string{"user", "group", "subscription_namespace"},
		},
		{
			name: "captureGroup true emits groups_str path",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureGroup: boolPtr(true),
				},
			},
			expectedLabels: map[string]any{
				"group": "auth.identity.groups_str",
			},
		},
		{
			name: "captureGroup false omits group label",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureGroup: boolPtr(false),
				},
			},
			absentKeys: []string{"group"},
		},
		{
			name: "captureUser true emits userid path",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureUser: boolPtr(true),
				},
			},
			expectedLabels: map[string]any{
				"user": "auth.identity.userid",
			},
		},
		{
			name: "captureOrganization false omits organization_id",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureOrganization: boolPtr(false),
				},
			},
			absentKeys: []string{"organization_id"},
		},
		{
			name: "captureModelUsage false omits model",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureModelUsage: boolPtr(false),
				},
			},
			absentKeys: []string{"model"},
		},
		{
			name: "all flags enabled",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureGroup:        boolPtr(true),
					CaptureUser:         boolPtr(true),
					CaptureOrganization: boolPtr(true),
					CaptureModelUsage:   boolPtr(true),
				},
			},
			expectedLabels: map[string]any{
				"tenant_name":      `"models-as-a-service"`,
				"tenant_namespace": `"models-as-a-service"`,
				"subscription":     "auth.identity.selected_subscription",
				"cost_center":      `has(auth.identity.subscription_info.costCenter) ? auth.identity.subscription_info.costCenter : ""`,
				"organization_id":  `has(auth.identity.subscription_info.organizationId) ? auth.identity.subscription_info.organizationId : ""`,
				"user":             "auth.identity.userid",
				"group":            "auth.identity.groups_str",
				"model":            modelLabel,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := buildTelemetryLabels(logr.Discard(), tt.config, telemetryTenant)

			for k, v := range tt.expectedLabels {
				assert.Equal(t, v, labels[k], "label %q", k)
			}
			for _, k := range tt.absentKeys {
				assert.NotContains(t, labels, k, "label %q should be absent", k)
			}
		})
	}
}

func TestConfigureTelemetryPolicyResourcesIncludesProducerScope(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelTenantName:      "redteam",
				LabelTenantNamespace: "ai-tenant-redteam",
			},
		},
	}
	var resources []unstructured.Unstructured
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "redteam",
		TenantIdentifier: "redteam",
	}

	require.NoError(t, configureTelemetryPolicyResources(logr.Discard(), tenant, &resources, params))
	require.Len(t, resources, 1)

	labels, found, err := unstructured.NestedMap(resources[0].Object,
		"spec", "metrics", "default", "labels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, `"redteam"`, labels["tenant_name"])
	assert.Equal(t, `"ai-tenant-redteam"`, labels["tenant_namespace"])
	assert.Equal(t, "auth.identity.selected_subscription", labels["subscription"])
	assert.Equal(t, CELResolvedModelIdentity, labels["model"])
	assert.NotContains(t, labels, "subscription_namespace")
	assert.Contains(t, labels, "organization_id")
}

func TestBuildIstioTelemetryTagOverridesIncludesProducerScope(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels:    map[string]string{LabelTenantName: "redteam"},
		},
	}
	overrides := buildIstioTelemetryTagOverrides(tenant, nil)
	assert.Equal(t, map[string]any{
		"operation": "UPSERT",
		"value":     `"redteam"`,
	}, overrides["tenant_name"])
	assert.Equal(t, map[string]any{
		"operation": "UPSERT",
		"value":     `"ai-tenant-redteam"`,
	}, overrides["tenant_namespace"])
	assert.Equal(t, map[string]any{
		"operation": "UPSERT",
		"value":     `request.headers["x-maas-subscription"]`,
	}, overrides["subscription"])
	assert.NotContains(t, overrides, "subscription_namespace")
	assert.Equal(t, map[string]any{
		"operation": "UPSERT",
		"value":     `request.headers["x-maas-organization-id"]`,
	}, overrides["organization_id"])
	assert.NotContains(t, overrides, "user")

	overrides = buildIstioTelemetryTagOverrides(tenant, &maasv1alpha1.TenantTelemetryConfig{
		Metrics: &maasv1alpha1.TenantMetricsConfig{CaptureUser: boolPtr(true)},
	})
	assert.NotContains(t, overrides, "user")

	overrides = buildIstioTelemetryTagOverrides(tenant, &maasv1alpha1.TenantTelemetryConfig{
		Metrics: &maasv1alpha1.TenantMetricsConfig{CaptureOrganization: boolPtr(false)},
	})
	assert.NotContains(t, overrides, "organization_id")

	overrides = buildIstioTelemetryTagOverrides(tenant, nil)
	assert.NotContains(t, overrides, "user")
}

func TestConfigurePayloadProcessingHPA(t *testing.T) {
	t.Run("no HPA appended when autoscaling disabled", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:             "openshift-ingress",
			PayloadProcessingAutoscaling: false,
		}
		err := configurePayloadProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		assert.Empty(t, resources)
	})

	t.Run("HPA appended with correct defaults", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:              "openshift-ingress",
			PayloadProcessingAutoscaling:  true,
			PayloadProcessingMaxReplicas:  10,
			PayloadProcessingTargetCPU:    70,
			PayloadProcessingTargetMemory: 80,
		}
		err := configurePayloadProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		require.Len(t, resources, 1)

		hpa := resources[0]
		assert.Equal(t, "HorizontalPodAutoscaler", hpa.GetKind())
		assert.Equal(t, "payload-processing", hpa.GetName())
		assert.Equal(t, "openshift-ingress", hpa.GetNamespace())

		targetName, _, _ := unstructured.NestedString(hpa.Object, "spec", "scaleTargetRef", "name")
		assert.Equal(t, "payload-processing", targetName)

		minReplicas, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "minReplicas")
		assert.Equal(t, int64(1), minReplicas)

		maxReplicas, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "maxReplicas")
		assert.Equal(t, int64(10), maxReplicas)
	})

	t.Run("HPA uses replicas as minReplicas", func(t *testing.T) {
		var resources []unstructured.Unstructured
		replicas := int32(3)
		params := PlatformParams{
			GatewayNamespace:              "openshift-ingress",
			PayloadProcessingAutoscaling:  true,
			PayloadProcessingReplicas:     &replicas,
			PayloadProcessingMaxReplicas:  15,
			PayloadProcessingTargetCPU:    60,
			PayloadProcessingTargetMemory: 75,
		}
		err := configurePayloadProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		require.Len(t, resources, 1)

		hpa := resources[0]
		minR, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "minReplicas")
		assert.Equal(t, int64(3), minR)

		maxR, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "maxReplicas")
		assert.Equal(t, int64(15), maxR)
	})

	t.Run("HPA uses tenant-specific names for non-default tenant", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:              "openshift-ingress",
			TenantIdentifier:              "redteam",
			PayloadProcessingAutoscaling:  true,
			PayloadProcessingMaxReplicas:  10,
			PayloadProcessingTargetCPU:    70,
			PayloadProcessingTargetMemory: 80,
		}
		err := configurePayloadProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		require.Len(t, resources, 1)

		hpa := resources[0]
		assert.Equal(t, "payload-processing-redteam", hpa.GetName())
		targetName, _, _ := unstructured.NestedString(hpa.Object, "spec", "scaleTargetRef", "name")
		assert.Equal(t, "payload-processing-redteam", targetName)
	})
}

func TestCleanupPayloadProcessingHPA(t *testing.T) {
	t.Run("no-op when autoscaling is enabled", func(t *testing.T) {
		params := PlatformParams{
			GatewayNamespace:             "openshift-ingress",
			PayloadProcessingAutoscaling: true,
		}
		// Should return nil without attempting any API calls
		err := cleanupPayloadProcessingHPA(context.Background(), nil, params, logr.Discard())
		require.NoError(t, err)
	})
}

func TestConfigurePayloadPreProcessingHPA(t *testing.T) {
	t.Run("no HPA appended when autoscaling disabled", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:                "openshift-ingress",
			PayloadPreProcessingAutoscaling: false,
		}
		err := configurePayloadPreProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		assert.Empty(t, resources)
	})

	t.Run("HPA appended with correct defaults", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:                 "openshift-ingress",
			PayloadPreProcessingAutoscaling:  true,
			PayloadPreProcessingMaxReplicas:  10,
			PayloadPreProcessingTargetCPU:    70,
			PayloadPreProcessingTargetMemory: 80,
		}
		err := configurePayloadPreProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		require.Len(t, resources, 1)

		hpa := resources[0]
		assert.Equal(t, "HorizontalPodAutoscaler", hpa.GetKind())
		assert.Equal(t, "payload-pre-processing", hpa.GetName())
		assert.Equal(t, "openshift-ingress", hpa.GetNamespace())

		targetName, _, _ := unstructured.NestedString(hpa.Object, "spec", "scaleTargetRef", "name")
		assert.Equal(t, "payload-pre-processing", targetName)

		minReplicas, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "minReplicas")
		assert.Equal(t, int64(1), minReplicas)

		maxReplicas, _, _ := unstructured.NestedInt64(hpa.Object, "spec", "maxReplicas")
		assert.Equal(t, int64(10), maxReplicas)
	})

	t.Run("HPA uses tenant-specific names for non-default tenant", func(t *testing.T) {
		var resources []unstructured.Unstructured
		params := PlatformParams{
			GatewayNamespace:                 "openshift-ingress",
			TenantIdentifier:                 "redteam",
			PayloadPreProcessingAutoscaling:  true,
			PayloadPreProcessingMaxReplicas:  10,
			PayloadPreProcessingTargetCPU:    70,
			PayloadPreProcessingTargetMemory: 80,
		}
		err := configurePayloadPreProcessingHPA(logr.Discard(), &resources, params)
		require.NoError(t, err)
		require.Len(t, resources, 1)

		hpa := resources[0]
		assert.Equal(t, "payload-pre-processing-redteam", hpa.GetName())
		targetName, _, _ := unstructured.NestedString(hpa.Object, "spec", "scaleTargetRef", "name")
		assert.Equal(t, "payload-pre-processing-redteam", targetName)
	})
}

func TestCleanupPayloadPreProcessingHPA(t *testing.T) {
	t.Run("no-op when autoscaling is enabled", func(t *testing.T) {
		params := PlatformParams{
			GatewayNamespace:                "openshift-ingress",
			PayloadPreProcessingAutoscaling: true,
		}
		err := cleanupPayloadPreProcessingHPA(context.Background(), nil, params, logr.Discard())
		require.NoError(t, err)
	})
}

func TestIsIPPResource(t *testing.T) {
	tests := []struct {
		name     string
		gvk      schema.GroupVersionKind
		resName  string
		expected bool
	}{
		{name: "payload-processing deployment", gvk: GVKDeployment, resName: PayloadProcessingName, expected: true},
		{name: "payload-pre-processing deployment", gvk: GVKDeployment, resName: PayloadPreProcessingName, expected: true},
		{name: "payload-processing envoy filter", gvk: GVKEnvoyFilter, resName: PayloadProcessingName, expected: true},
		{name: "payload-processing plugins configmap", gvk: GVKConfigMap, resName: PayloadProcessingPluginsConfigMapName, expected: true},
		{name: "maas-api deployment", gvk: GVKDeployment, resName: baseMaaSAPIDeploymentName, expected: false},
		{name: "gateway default deny policy", gvk: GVKTokenRateLimitPolicy, resName: baseGatewayTokenRateLimitDefaultDenyPolicyName, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isIPPResource(tt.gvk, tt.resName))
		})
	}
}

func staticTelemetryModelLabel(t *testing.T) string {
	t.Helper()
	policy, err := os.ReadFile("../../../../deployment/base/observability/gateway-telemetry-policy.yaml")
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, yaml.Unmarshal(policy, &object))
	staticExpression, found, err := unstructured.NestedString(object, "spec", "metrics", "default", "labels", "model")
	require.NoError(t, err)
	require.True(t, found)
	return staticExpression
}

// No response context is provided: errors may have plain-text, HTML or empty
// bodies, and a backend's model name can differ from the published model alias.
func TestTelemetryModelLabelUsesPublishedRequestModel(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "models-as-a-service",
		},
	}
	expression, ok := buildTelemetryLabels(logr.Discard(), nil, tenant)["model"].(string)
	require.True(t, ok)
	require.Equal(t, CELResolvedModelIdentity, expression)
	require.NotContains(t, expression, "auth.metadata", "Limitador must use exported identity metadata")
	require.Equal(t, staticTelemetryModelLabel(t), expression, "static and tenant-rendered model labels must stay in sync")
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("auth", cel.DynType), ext.Strings())
	require.NoError(t, err)
	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)

	tests := []struct {
		name     string
		path     string
		headers  map[string]string
		expected string
	}{
		{"published alias without a response model", "/v1/chat/completions", map[string]string{"x-gateway-model-name": "public-chat"}, "public-chat"},
		{"publisher model identifier", "/v1/chat/completions", map[string]string{"x-gateway-model-name": "publishers/llm/models/gpu-llama"}, "publishers/llm/models/gpu-llama"},
		{"path identity overrides a conflicting body model", "/llm/public-chat/v1/chat/completions", map[string]string{"x-gateway-model-name": "unrelated-body-model"}, "llm/public-chat"},
		{"path identity without a body model", "/llm/public-chat/v1/chat/completions", map[string]string{}, "llm/public-chat"},
		{"empty path segments follow authorization identity", "//llm//public-chat/v1/chat/completions", map[string]string{}, "llm/public-chat"},
		{"management request without a model", "/maas-api/v1/models", map[string]string{}, ""},
		{"catalog request without a model", "/v1/models", map[string]string{}, ""},
		{"root request without a model", "/", map[string]string{}, ""},
		{"short path without a model", "/health", map[string]string{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{"request": map[string]any{"path": tt.path, "headers": tt.headers}, "auth": map[string]any{}})
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result.Value())
		})
	}
	for _, request := range []map[string]any{
		{"path": "/llm/public-chat/v1/chat/completions", "headers": map[string]string{}},
		{"path": "/v1/chat/completions", "headers": map[string]string{"x-gateway-model-name": "publishers/llm/models/public-chat"}},
		{"path": "/v1/chat/completions", "headers": map[string]string{"x-gateway-model-name": "another-alias"}},
	} {
		result, _, err := program.Eval(map[string]any{
			"request": request,
			"auth":    map[string]any{"identity": map[string]any{"subscription_info": map[string]any{"resolvedModel": "llm/canonical-ref"}}},
		})
		require.NoError(t, err)
		assert.Equal(t, "llm/canonical-ref", result.Value(), "aliases must share the limiter's canonical model label")
	}
	for _, auth := range []any{
		nil,
		map[string]any{},
		map[string]any{"identity": map[string]any{}},
		map[string]any{"identity": map[string]any{"subscription_info": map[string]any{"resolvedModel": ""}}},
	} {
		result, _, err := program.Eval(map[string]any{
			"request": map[string]any{"path": "/llm/public-chat/v1/chat/completions", "headers": map[string]string{}},
			"auth":    auth,
		})
		require.NoError(t, err)
		assert.Equal(t, "llm/public-chat", result.Value(), "requests before subscription selection retain their fallback identity")
	}
}
