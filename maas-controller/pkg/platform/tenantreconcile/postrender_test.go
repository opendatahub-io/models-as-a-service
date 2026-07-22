package tenantreconcile

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestBuildTelemetryLabels(t *testing.T) {
	tests := []struct {
		name          string
		config        *maasv1alpha1.TenantTelemetryConfig
		expectModel   bool
		expectUser    bool
		expectGroup   bool
		expectOrgID   bool
		alwaysPresent []string
	}{
		{
			name: "defaults include model but not user/group",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(true),
			},
			expectModel: true,
			expectUser:  false,
			expectGroup: false,
			expectOrgID: true,
			alwaysPresent: []string{"subscription", "cost_center"},
		},
		{
			name: "model disabled excludes model label",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(true),
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureModelUsage: ptr.To(false),
				},
			},
			expectModel: false,
			expectUser:  false,
			expectGroup: false,
			expectOrgID: true,
			alwaysPresent: []string{"subscription", "cost_center"},
		},
		{
			name: "all labels enabled",
			config: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(true),
				Metrics: &maasv1alpha1.TenantMetricsConfig{
					CaptureModelUsage:   ptr.To(true),
					CaptureUser:         ptr.To(true),
					CaptureGroup:        ptr.To(true),
					CaptureOrganization: ptr.To(true),
				},
			},
			expectModel: true,
			expectUser:  true,
			expectGroup: true,
			expectOrgID: true,
			alwaysPresent: []string{"subscription", "cost_center"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := buildTelemetryLabels(logr.Discard(), tt.config)

			if tt.expectModel {
				assert.Contains(t, labels, "model")
				assert.Equal(t, `responseBodyJSON("/model")`, labels["model"])
			} else {
				assert.NotContains(t, labels, "model")
			}

			if tt.expectUser {
				assert.Contains(t, labels, "user")
			} else {
				assert.NotContains(t, labels, "user")
			}

			if tt.expectGroup {
				assert.Contains(t, labels, "group")
			} else {
				assert.NotContains(t, labels, "group")
			}

			if tt.expectOrgID {
				assert.Contains(t, labels, "organization_id")
			} else {
				assert.NotContains(t, labels, "organization_id")
			}

			for _, key := range tt.alwaysPresent {
				assert.Contains(t, labels, key)
			}
		})
	}
}

func TestBuildManagementTelemetryLabels_ExcludesModel(t *testing.T) {
	config := &maasv1alpha1.TenantTelemetryConfig{
		Enabled: ptr.To(true),
		Metrics: &maasv1alpha1.TenantMetricsConfig{
			CaptureModelUsage:   ptr.To(true),
			CaptureUser:         ptr.To(true),
			CaptureOrganization: ptr.To(true),
		},
	}

	inferenceLabels := buildTelemetryLabels(logr.Discard(), config)
	assert.Contains(t, inferenceLabels, "model", "inference labels should include model")

	mgmtLabels := buildManagementTelemetryLabels(logr.Discard(), config)
	assert.NotContains(t, mgmtLabels, "model", "management labels must not include model")

	assert.Contains(t, mgmtLabels, "subscription")
	assert.Contains(t, mgmtLabels, "cost_center")
	assert.Contains(t, mgmtLabels, "organization_id")
	assert.Contains(t, mgmtLabels, "user")
}

func TestConfigureTelemetryPolicyResources(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-tenant",
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.TenantSpec{
			Telemetry: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(true),
			},
		},
	}
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
		AppNamespace:     "opendatahub",
		TenantIdentifier: "",
	}

	var resources []unstructured.Unstructured
	err := configureTelemetryPolicyResources(logr.Discard(), tenant, &resources, params)
	require.NoError(t, err)
	require.Len(t, resources, 2, "should create both gateway and management TelemetryPolicies")

	gwTP := resources[0]
	assert.Equal(t, "TelemetryPolicy", gwTP.GetKind())
	assert.Equal(t, "maas-telemetry", gwTP.GetName())
	assert.Equal(t, "openshift-ingress", gwTP.GetNamespace())

	gwTargetKind, _, _ := unstructured.NestedString(gwTP.Object, "spec", "targetRef", "kind")
	assert.Equal(t, "Gateway", gwTargetKind)
	gwTargetName, _, _ := unstructured.NestedString(gwTP.Object, "spec", "targetRef", "name")
	assert.Equal(t, "maas-default-gateway", gwTargetName)

	gwLabels, _, _ := unstructured.NestedMap(gwTP.Object, "spec", "metrics", "default", "labels")
	assert.Contains(t, gwLabels, "model", "gateway TelemetryPolicy should include model label")

	mgmtTP := resources[1]
	assert.Equal(t, "TelemetryPolicy", mgmtTP.GetKind())
	assert.Equal(t, "maas-mgmt-telemetry", mgmtTP.GetName())
	assert.Equal(t, "opendatahub", mgmtTP.GetNamespace())

	mgmtTargetKind, _, _ := unstructured.NestedString(mgmtTP.Object, "spec", "targetRef", "kind")
	assert.Equal(t, "HTTPRoute", mgmtTargetKind)
	mgmtTargetName, _, _ := unstructured.NestedString(mgmtTP.Object, "spec", "targetRef", "name")
	assert.Equal(t, "maas-api-route", mgmtTargetName)

	mgmtLabels, _, _ := unstructured.NestedMap(mgmtTP.Object, "spec", "metrics", "default", "labels")
	assert.NotContains(t, mgmtLabels, "model", "management TelemetryPolicy must not include model label")
	assert.Contains(t, mgmtLabels, "subscription")
}

func TestConfigureTelemetryPolicyResources_MultiTenant(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-tenant",
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "redteam",
				LabelTenantNamespace:   "ai-tenant-redteam",
			},
		},
		Spec: maasv1alpha1.TenantSpec{
			Telemetry: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(true),
			},
		},
	}
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
		AppNamespace:     "opendatahub",
		TenantIdentifier: "redteam",
	}

	var resources []unstructured.Unstructured
	err := configureTelemetryPolicyResources(logr.Discard(), tenant, &resources, params)
	require.NoError(t, err)
	require.Len(t, resources, 2)

	assert.Equal(t, "maas-telemetry-redteam", resources[0].GetName())
	assert.Equal(t, "maas-mgmt-telemetry-redteam", resources[1].GetName())

	mgmtTargetName, _, _ := unstructured.NestedString(resources[1].Object, "spec", "targetRef", "name")
	assert.Equal(t, "maas-api-route-redteam", mgmtTargetName)
}

func TestConfigureTelemetryPolicyResources_DisabledSkipsBoth(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-tenant",
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.TenantSpec{
			Telemetry: &maasv1alpha1.TenantTelemetryConfig{
				Enabled: ptr.To(false),
			},
		},
	}
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
		AppNamespace:     "opendatahub",
	}

	var resources []unstructured.Unstructured
	err := configureTelemetryPolicyResources(logr.Discard(), tenant, &resources, params)
	require.NoError(t, err)
	assert.Empty(t, resources, "disabled telemetry should produce no resources")
}
