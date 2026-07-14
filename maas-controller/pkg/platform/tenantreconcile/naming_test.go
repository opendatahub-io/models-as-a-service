package tenantreconcile

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestTenantIdentifierFor(t *testing.T) {
	tests := []struct {
		name     string
		tenant   *maasv1alpha1.Tenant
		expected string
	}{
		{
			name:     "nil tenant returns empty string",
			tenant:   nil,
			expected: "",
		},
		{
			name: "legacy tenant without AITenant label returns empty string",
			tenant: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "models-as-a-service",
				},
			},
			expected: "",
		},
		{
			name: "AITenant-managed tenant returns tenant name from label",
			tenant: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "ai-tenant-redteam",
					Labels: map[string]string{
						LabelManagedByAITenant: "true",
						LabelTenantName:        "redteam",
						LabelTenantNamespace:   "ai-tenant-redteam",
					},
				},
			},
			expected: "redteam",
		},
		{
			name: "default AITenant-managed tenant keeps legacy empty identifier",
			tenant: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "models-as-a-service",
					Labels: map[string]string{
						LabelManagedByAITenant: "true",
						LabelTenantName:        DefaultAITenantName,
						LabelTenantNamespace:   "models-as-a-service",
					},
				},
			},
			expected: "",
		},
		{
			name: "AITenant-managed tenant with different name",
			tenant: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "ai-tenant-engineering",
					Labels: map[string]string{
						LabelManagedByAITenant: "true",
						LabelTenantName:        "engineering",
					},
				},
			},
			expected: "engineering",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TenantIdentifierFor(tt.tenant)
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}

	// Separate test for error case: AITenant-managed without tenant name
	t.Run("tenant with AITenant label but no tenant name returns error", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-tenant",
				Namespace: "ai-tenant-broken",
				Labels: map[string]string{
					LabelManagedByAITenant: "true",
					// Missing LabelTenantName - should error
				},
			},
		}
		_, err := TenantIdentifierFor(tenant)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tenant ai-tenant-broken/default-tenant has maas.opendatahub.io/managed-by-aitenant=true but maas.opendatahub.io/tenant-name is missing")
	})

	t.Run("default AITenant label without matching tenant namespace returns error", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      maasv1alpha1.TenantInstanceName,
				Namespace: "ai-tenant-spoofed",
				Labels: map[string]string{
					LabelManagedByAITenant: "true",
					LabelTenantName:        DefaultAITenantName,
				},
			},
		}
		_, err := TenantIdentifierFor(tenant)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "maas.opendatahub.io/tenant-namespace must match the tenant config namespace")
	})

	t.Run("default AITenant label on non-default tenant config resource returns error", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "spoofed",
				Namespace: "models-as-a-service",
				Labels: map[string]string{
					LabelManagedByAITenant: "true",
					LabelTenantName:        DefaultAITenantName,
					LabelTenantNamespace:   "models-as-a-service",
				},
			},
		}
		_, err := TenantIdentifierFor(tenant)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "is not the default tenant config resource")
	})
}

func TestResourceNamingFunctions(t *testing.T) {
	tests := []struct {
		name             string
		tenantID         string
		expectedDepl     string
		expectedSvc      string
		expectedRoute    string
		expectedAuth     string
		expectedCron     string
		expectedDestRule string
	}{
		{
			name:             "default tenant (empty string) uses base names",
			tenantID:         "",
			expectedDepl:     "maas-api",
			expectedSvc:      "maas-api",
			expectedRoute:    "maas-api-route",
			expectedAuth:     "maas-api-auth-policy",
			expectedCron:     "maas-api-key-cleanup",
			expectedDestRule: "maas-api-backend-tls",
		},
		{
			name:             "redteam tenant gets suffixed names",
			tenantID:         "redteam",
			expectedDepl:     "maas-api-redteam",
			expectedSvc:      "maas-api-redteam",
			expectedRoute:    "maas-api-route-redteam",
			expectedAuth:     "maas-api-auth-policy-redteam",
			expectedCron:     "maas-api-key-cleanup-redteam",
			expectedDestRule: "maas-api-backend-tls-redteam",
		},
		{
			name:             "engineering tenant gets suffixed names",
			tenantID:         "engineering",
			expectedDepl:     "maas-api-engineering",
			expectedSvc:      "maas-api-engineering",
			expectedRoute:    "maas-api-route-engineering",
			expectedAuth:     "maas-api-auth-policy-engineering",
			expectedCron:     "maas-api-key-cleanup-engineering",
			expectedDestRule: "maas-api-backend-tls-engineering",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectedDepl, MaaSAPIDeploymentName(tt.tenantID))
			assert.Equal(t, tt.expectedSvc, MaaSAPIServiceName(tt.tenantID))
			assert.Equal(t, tt.expectedRoute, MaaSAPIRouteName(tt.tenantID))
			assert.Equal(t, tt.expectedAuth, MaaSAPIAuthPolicyName(tt.tenantID))
			assert.Equal(t, tt.expectedCron, MaaSAPIKeyCleanupCronJobName(tt.tenantID))
			assert.Equal(t, tt.expectedDestRule, GatewayDestinationRuleName(tt.tenantID))
		})
	}
}

func TestResourceNameLength(t *testing.T) {
	// Verify that even with max-length tenant name (41 chars),
	// resource names stay within Kubernetes 63-char limit
	maxLengthTenantName := "engineering-data-science-west-coast-t" // 41 chars (max allowed by AITenant CRD)

	tests := []struct {
		name         string
		resourceName string
		maxLength    int
	}{
		{"Deployment", MaaSAPIDeploymentName(maxLengthTenantName), 63},
		{"Service", MaaSAPIServiceName(maxLengthTenantName), 63},
		{"HTTPRoute", MaaSAPIRouteName(maxLengthTenantName), 63},
		{"AuthPolicy", MaaSAPIAuthPolicyName(maxLengthTenantName), 63},
		{"CronJob", MaaSAPIKeyCleanupCronJobName(maxLengthTenantName), 63},
		{"DestinationRule", GatewayDestinationRuleName(maxLengthTenantName), 63},
		{"TelemetryPolicy", TelemetryPolicyName(maxLengthTenantName), 63},
		{"IstioTelemetry", IstioTelemetryName(maxLengthTenantName), 63},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			length := len(tt.resourceName)
			assert.LessOrEqual(t, length, tt.maxLength,
				"Resource name %q exceeds %d char limit (length: %d)", tt.resourceName, tt.maxLength, length)
		})
	}
}

func TestBuildPlatformParamsIncludesTenantIdentifier(t *testing.T) {
	t.Run("legacy tenant without labels gets empty tenant identifier", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-tenant",
				Namespace: "models-as-a-service",
			},
			Spec: maasv1alpha1.TenantSpec{
				GatewayRef: maasv1alpha1.TenantGatewayRef{
					Namespace: "openshift-ingress",
					Name:      "maas-default-gateway",
				},
			},
		}

		platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Namespace: "openshift-ingress",
			Name:      "maas-default-gateway",
		}}
		params, err := BuildPlatformParams(tenant, platformContext, "opendatahub", "https://kubernetes.default.svc", logr.Discard())
		assert.NoError(t, err)

		assert.Equal(t, "", params.TenantIdentifier)
	})

	t.Run("default AITenant-managed tenant keeps empty tenant identifier", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-tenant",
				Namespace: "models-as-a-service",
				Labels: map[string]string{
					LabelManagedByAITenant: "true",
					LabelTenantName:        DefaultAITenantName,
					LabelTenantNamespace:   "models-as-a-service",
				},
			},
			Spec: maasv1alpha1.TenantSpec{
				GatewayRef: maasv1alpha1.TenantGatewayRef{
					Namespace: "openshift-ingress",
					Name:      "maas-default-gateway",
				},
			},
		}

		platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Namespace: "openshift-ingress",
			Name:      "maas-default-gateway",
		}}
		params, err := BuildPlatformParams(tenant, platformContext, "opendatahub", "https://kubernetes.default.svc", logr.Discard())
		assert.NoError(t, err)

		assert.Equal(t, "", params.TenantIdentifier)
	})

	t.Run("AITenant-managed tenant gets tenant name as identifier", func(t *testing.T) {
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-tenant",
				Namespace: "ai-tenant-redteam",
				Labels: map[string]string{
					LabelManagedByAITenant: "true",
					LabelTenantName:        "redteam",
				},
			},
			Spec: maasv1alpha1.TenantSpec{
				GatewayRef: maasv1alpha1.TenantGatewayRef{
					Namespace: "openshift-ingress",
					Name:      "redteam-gateway",
				},
			},
		}

		platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Namespace: "openshift-ingress",
			Name:      "redteam-gateway",
		}}
		params, err := BuildPlatformParams(tenant, platformContext, "opendatahub", "https://kubernetes.default.svc", logr.Discard())
		assert.NoError(t, err)

		assert.Equal(t, "redteam", params.TenantIdentifier)
	})
}
