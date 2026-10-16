package tenantreconcile

import (
	"context"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/groupname"
)

func boolPtr(b bool) *bool { return &b }

// TestPatchAuthPolicyWithOIDC_GroupsExprUsesCanonicalPattern ensures the
// per-tenant maas-api AuthPolicy post-render gates X-MaaS-Group-OC JSON
// construction on the canonical group-name pattern (maas-controller/pkg/
// groupname). This is the AuthPolicy-side twin of the maas-api API-key
// mint-time validation (maas-api/internal/api_keys/groupname.go).
func TestPatchAuthPolicyWithOIDC_GroupsExprUsesCanonicalPattern(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]any{}}
	// SetNestedField requires intermediate maps to exist; the rendered
	// maas-api AuthPolicy provides them in production.
	for _, path := range [][]string{
		{"spec", "rules", "authentication"},
		{"spec", "rules", "authentication", "openshift-identities"},
		{"spec", "rules", "authorization"},
		{"spec", "rules", "response", "success", "headers"},
	} {
		require.NoError(t, unstructured.SetNestedField(resource.Object, map[string]any{}, path...))
	}

	oidc := &maasv1alpha1.TenantExternalOIDCConfig{IssuerURL: "https://idp.example.com/realms/test", ClientID: "maas"}
	require.NoError(t, patchAuthPolicyWithOIDC(logr.Discard(), resource, oidc))

	expr, found, err := unstructured.NestedString(resource.Object, "spec", "rules", "response", "success", "headers", "X-MaaS-Group-OC", "plain", "expression")
	require.NoError(t, err)
	require.True(t, found, "X-MaaS-Group-OC expression should be set")
	assert.Contains(t, expr, "g.matches('"+groupname.Pattern+"')",
		"OIDC groups must be gated on the canonical group-name pattern")
	assert.Contains(t, expr, `'["system:authenticated","' + auth.identity.groups.join('","') + '"]'`,
		"safe groups must be joined with the implicit system:authenticated prefix")
	// The pattern must accept slash groups (IdP full-path claims) while the
	// expression still falls back for unsafe ones.
	assert.Regexp(t, `/`, groupname.Pattern, "canonical pattern must allow slashes")
	assert.True(t, groupname.Valid("/Engineering"))
	assert.False(t, groupname.Valid(`bad"group`))

	// Security: the group source is selected by identity type, never by claim
	// presence — a JWT carrying a crafted "user" claim must never reach the
	// TokenReview branch — and both sources are gated on the pattern.
	assert.Contains(t, expr, "has(auth.identity.iss)",
		"JWT identities must be selected by iss, not by groups-claim presence")
	assert.Contains(t, expr, "auth.identity.user.groups.all(g, g.matches('"+groupname.Pattern+"')",
		"TokenReview groups must be gated on the canonical group-name pattern")
}

func TestPatchAuthPolicyWithOIDCRequiresPurposePair(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	oidc := &maasv1alpha1.TenantExternalOIDCConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
	}

	err := patchAuthPolicyWithOIDC(logr.Discard(), resource, oidc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be configured together")
}

func TestPatchAuthPolicyWithOIDCTokenContract(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	oidc := &maasv1alpha1.TenantExternalOIDCConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-api",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
	}

	require.NoError(t, patchAuthPolicyWithOIDC(logr.Discard(), resource, oidc))
	patterns, found, err := unstructured.NestedSlice(
		resource.Object,
		"spec", "rules", "authorization", "oidc-client-bound", "patternMatching", "patterns",
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, patterns, 4)

	expected := []map[string]string{
		{"selector": "auth.identity.iss", "operator": "eq", "value": oidc.IssuerURL},
		{"selector": "auth.identity.azp", "operator": "eq", "value": oidc.ClientID},
		{"selector": "auth.identity.token_use", "operator": "eq", "value": oidc.AccessTokenPurpose},
	}
	for index, patternIndex := range []int{0, 1, 3} {
		patternMap, ok := patterns[patternIndex].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, expected[index]["selector"], patternMap["selector"])
		assert.Equal(t, expected[index]["operator"], patternMap["operator"])
		assert.Equal(t, expected[index]["value"], patternMap["value"])
	}

	audiencePattern, ok := patterns[2].(map[string]any)
	require.True(t, ok)
	assert.Equal(t,
		`has(auth.identity.aud) && (auth.identity.aud == "maas-api" || "maas-api" in auth.identity.aud)`,
		audiencePattern["predicate"],
	)
}

func TestDefaultDenyStateOperations(t *testing.T) {
	data, err := os.ReadFile("../../../../deployment/base/maas-controller/policies/gateway-default-deny.yaml")
	require.NoError(t, err)
	var policy map[string]any
	require.NoError(t, yaml.Unmarshal(data, &policy))
	when, found, err := unstructured.NestedSlice(policy, "spec", "defaults", "limits", "deny-all-by-default", "when")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, when)
	entry, ok := when[0].(map[string]any)
	require.True(t, ok)
	predicate, ok := entry["predicate"].(string)
	require.True(t, ok)
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType))
	require.NoError(t, err)
	ast, issues := env.Compile(predicate)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)
	for _, tt := range []struct {
		method, path string
		deny         bool
	}{
		{"POST", "/v1/conversations", false},
		{"POST", "/v1/conversations/conv_test/items", false},
		{"GET", "/v1/conversations/conv_test", false},
		{"DELETE", "/v1/conversations/conv_test", false},
		{"GET", "/v1/responses/resp_test", false},
		{"GET", "/v1/responses/resp_test/input_items", false},
		{"DELETE", "/v1/responses/resp_test", false},
		{"POST", "/v1/responses", true},
		{"POST", "/v1/responses/", true},
		{"POST", "/v1/responses/resp_test/cancel", true},
		{"POST", "/v1/conversations-forged", true},
		{"PUT", "/v1/conversations/conv_test", true},
		{"POST", "/tenant/model/v1/chat/completions", true},
		{"GET", "/v1/models", false},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			out, _, err := program.Eval(map[string]any{"request": map[string]any{"method": tt.method, "path": tt.path}})
			require.NoError(t, err)
			assert.Equal(t, tt.deny, out.Value())
		})
	}
}

func TestBuildTelemetryLabels(t *testing.T) {
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
				"subscription":    "auth.identity.selected_subscription",
				"cost_center":     `has(auth.identity.subscription_info.costCenter) ? auth.identity.subscription_info.costCenter : ""`,
				"organization_id": `has(auth.identity.subscription_info.organizationId) ? auth.identity.subscription_info.organizationId : ""`,
				"model":           "responseBodyJSON(\"/model\")",
			},
			absentKeys: []string{"user", "group"},
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
				"subscription":    "auth.identity.selected_subscription",
				"cost_center":     `has(auth.identity.subscription_info.costCenter) ? auth.identity.subscription_info.costCenter : ""`,
				"organization_id": `has(auth.identity.subscription_info.organizationId) ? auth.identity.subscription_info.organizationId : ""`,
				"user":            "auth.identity.userid",
				"group":           "auth.identity.groups_str",
				"model":           "responseBodyJSON(\"/model\")",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := buildTelemetryLabels(logr.Discard(), tt.config)

			for k, v := range tt.expectedLabels {
				assert.Equal(t, v, labels[k], "label %q", k)
			}
			for _, k := range tt.absentKeys {
				assert.NotContains(t, labels, k, "label %q should be absent", k)
			}
		})
	}
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
