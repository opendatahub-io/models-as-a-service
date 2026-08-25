package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func boolPtr(b bool) *bool { return &b }

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
