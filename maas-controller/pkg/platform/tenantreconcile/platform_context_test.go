package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
