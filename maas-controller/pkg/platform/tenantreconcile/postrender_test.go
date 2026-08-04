package tenantreconcile

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"

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
