package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func platformContextTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, maasv1alpha1.AddToScheme(scheme))
	return scheme
}

func TestResolvePlatformContext_AITenantManagedTenantUsesAITenant(t *testing.T) {
	scheme := platformContextTestScheme(t)
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "redteam",
				LabelTenantNamespace:   "ai-tenant-redteam",
			},
			Annotations: map[string]string{
				AnnotationAITenantName:      "redteam",
				AnnotationAITenantNamespace: DefaultAITenantNamespace,
			},
		},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "stale-gateway-ns",
				Name:      "stale-gateway",
			},
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://stale.example.com",
				ClientID:  "stale-client",
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "redteam", Namespace: DefaultAITenantNamespace},
		Spec: maasv1alpha1.AITenantSpec{
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/redteam",
				ClientID:  "redteam-client",
			},
		},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "redteam-gateway",
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, aitenant).Build()

	got, err := ResolvePlatformContext(context.Background(), client, tenant, maasv1alpha1.TenantGatewayRef{
		Namespace: "fallback-ns",
		Name:      "fallback-gateway",
	})

	require.NoError(t, err)
	assert.Equal(t, maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "redteam-gateway"}, got.GatewayRef)
	require.NotNil(t, got.ExternalOIDC)
	assert.Equal(t, "https://issuer.example.com/realms/redteam", got.ExternalOIDC.IssuerURL)
	assert.Equal(t, "redteam-client", got.ExternalOIDC.ClientID)
	assert.Equal(t, "aitenant", got.Source)
	assert.False(t, got.SkipIPP)
}

func TestResolvePlatformContext_TenantConfigAnnotationSkipsIPP(t *testing.T) {
	scheme := platformContextTestScheme(t)
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "redteam",
				LabelTenantNamespace:   "ai-tenant-redteam",
			},
			Annotations: map[string]string{
				AnnotationAITenantName:          "redteam",
				AnnotationAITenantNamespace:     DefaultAITenantNamespace,
				AnnotationPayloadProcessingType: PayloadProcessingTypePraxis,
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redteam",
			Namespace: DefaultAITenantNamespace,
		},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "redteam-gateway",
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, aitenant).Build()

	got, err := ResolvePlatformContext(context.Background(), client, tenant, maasv1alpha1.TenantGatewayRef{})
	require.NoError(t, err)
	assert.True(t, got.SkipIPP)
}

// TestResolveSkipIPP_AITenantAnnotationIsIgnored asserts that
// maas.opendatahub.io/payload-processing-type is read exclusively from the tenant
// config object (MaasTenantConfig): it is never mirrored to/from AITenant, so a value
// set directly on AITenant (e.g. by an operator who has not updated the tenant config)
// must have no effect on dataplane selection.
func TestResolveSkipIPP_AITenantAnnotationIsIgnored(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationPayloadProcessingType: "unknown",
			},
		},
	}

	assert.False(t, resolveSkipIPP(tenant))
}

func TestResolveSkipIPP_AbsentAnnotationMeansIPP(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{}
	assert.False(t, resolveSkipIPP(tenant))
}

func TestResolveSkipIPP_PraxisAnnotationSkipsIPP(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnotationPayloadProcessingType: PayloadProcessingTypePraxis,
			},
		},
	}
	assert.True(t, resolveSkipIPP(tenant))
}

func TestResolvePlatformContext_LegacyTenantUsesTenantSpec(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: "models-as-a-service"},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "custom-ingress",
				Name:      "custom-gateway",
			},
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/default",
				ClientID:  "default-client",
			},
		},
	}

	got, err := ResolvePlatformContext(context.Background(), nil, tenant, maasv1alpha1.TenantGatewayRef{
		Namespace: "fallback-ns",
		Name:      "fallback-gateway",
	})

	require.NoError(t, err)
	assert.Equal(t, maasv1alpha1.TenantGatewayRef{Namespace: "custom-ingress", Name: "custom-gateway"}, got.GatewayRef)
	require.NotNil(t, got.ExternalOIDC)
	assert.Equal(t, "default-client", got.ExternalOIDC.ClientID)
	assert.Equal(t, "legacy-tenant-spec", got.Source)
	assert.False(t, got.SkipIPP)
}

func TestResolvePlatformContext_AITenantStatusGatewayRequired(t *testing.T) {
	scheme := platformContextTestScheme(t)
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "redteam",
			},
			Annotations: map[string]string{
				AnnotationAITenantName:      "redteam",
				AnnotationAITenantNamespace: DefaultAITenantNamespace,
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "redteam", Namespace: DefaultAITenantNamespace},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, aitenant).Build()

	_, err := ResolvePlatformContext(context.Background(), client, tenant, maasv1alpha1.TenantGatewayRef{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "status.gatewayRef is not ready")
}

func TestResolvePlatformContext_AITenantNameAnnotationRequired(t *testing.T) {
	scheme := platformContextTestScheme(t)
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "redteam",
			},
			Annotations: map[string]string{
				AnnotationAITenantNamespace: DefaultAITenantNamespace,
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

	_, err := ResolvePlatformContext(context.Background(), client, tenant, maasv1alpha1.TenantGatewayRef{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), AnnotationAITenantName)
}

func TestResolvePlatformContext_MarksCausesAWatchReports(t *testing.T) {
	scheme := platformContextTestScheme(t)
	managed := func(labels, annotations map[string]string) *maasv1alpha1.MaasTenantConfig {
		labels[LabelManagedByAITenant] = "true"
		return &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "ai-tenant-team-a",
			Labels: labels, Annotations: annotations,
		}}
	}
	owner := map[string]string{AnnotationAITenantName: "team-a", AnnotationAITenantNamespace: "ai-tenants"}
	pendingAITenant := &maasv1alpha1.AITenant{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "ai-tenants"}}

	tests := []struct {
		name   string
		tenant *maasv1alpha1.MaasTenantConfig
		reader client.Reader
	}{
		{
			name:   "missing tenant-name label",
			tenant: managed(map[string]string{}, owner),
			reader: fake.NewClientBuilder().WithScheme(scheme).Build(),
		},
		{
			name:   "missing AITenant annotations",
			tenant: managed(map[string]string{LabelTenantName: "team-a"}, nil),
			reader: fake.NewClientBuilder().WithScheme(scheme).Build(),
		},
		{
			name:   "AITenant not created yet",
			tenant: managed(map[string]string{LabelTenantName: "team-a"}, owner),
			reader: fake.NewClientBuilder().WithScheme(scheme).Build(),
		},
		{
			name:   "AITenant status.gatewayRef not set yet",
			tenant: managed(map[string]string{LabelTenantName: "team-a"}, owner),
			reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pendingAITenant).Build(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolvePlatformContext(t.Context(), tt.reader, tt.tenant, maasv1alpha1.TenantGatewayRef{})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPlatformContextNotReady)
		})
	}

	t.Run("a failed AITenant read is not marked", func(t *testing.T) {
		readErr := errors.New("connection refused")
		reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return readErr
			},
		}).Build()
		_, err := ResolvePlatformContext(t.Context(), reader, managed(map[string]string{LabelTenantName: "team-a"}, owner), maasv1alpha1.TenantGatewayRef{})
		require.ErrorIs(t, err, readErr)
		assert.NotErrorIs(t, err, ErrPlatformContextNotReady)
	})
}
