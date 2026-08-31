package gatewayresolve

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

var testScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(testScheme))
}

func TestForNamespace_AITenantManaged(t *testing.T) {
	ctx := context.Background()
	const (
		tenantNS     = "ai-tenant-redteam"
		aitenantNS   = "ai-tenants"
		fallbackName = "maas-default-gateway"
		fallbackNS   = "openshift-ingress"
	)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "redteam", Namespace: aitenantNS},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "redteam-gateway",
				Namespace: "openshift-ingress",
			},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "redteam",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(aitenant, ns).Build()
	ref, err := ForNamespace(ctx, c, tenantNS, aitenantNS, "models-as-a-service", fallbackName, fallbackNS, true)
	if err != nil {
		t.Fatalf("ForNamespace() error = %v", err)
	}
	if ref.Name != "redteam-gateway" || ref.Namespace != "openshift-ingress" {
		t.Fatalf("ForNamespace() = %#v, want redteam-gateway/openshift-ingress", ref)
	}
}

func TestForNamespace_DefaultTenantFallback(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testScheme).Build()
	ref, err := ForNamespace(ctx, c, "models-as-a-service", "ai-tenants", "models-as-a-service", "maas-default-gateway", "openshift-ingress", true)
	if err != nil {
		t.Fatalf("ForNamespace() error = %v", err)
	}
	if ref.Name != "maas-default-gateway" || ref.Namespace != "openshift-ingress" {
		t.Fatalf("ForNamespace() = %#v, want default gateway fallback", ref)
	}
}

func TestForNamespace_DefaultTenantUsesAITenantWhenReady(t *testing.T) {
	ctx := context.Background()
	const defaultNS = "models-as-a-service"

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.DefaultAITenantName, Namespace: tenantreconcile.DefaultAITenantNamespace},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "maas-default-gateway",
				Namespace: "openshift-ingress",
			},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(aitenant, ns).Build()
	ref, err := ForNamespace(ctx, c, defaultNS, tenantreconcile.DefaultAITenantNamespace, defaultNS, "other-gateway", "other-ns", true)
	if err != nil {
		t.Fatalf("ForNamespace() error = %v", err)
	}
	if ref.Name != "maas-default-gateway" || ref.Namespace != "openshift-ingress" {
		t.Fatalf("ForNamespace() = %#v, want default AITenant gateway", ref)
	}
}

func TestForNamespace_DefaultTenantAITenantNotReadyFallsBack(t *testing.T) {
	ctx := context.Background()
	const defaultNS = "models-as-a-service"

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.DefaultAITenantName, Namespace: tenantreconcile.DefaultAITenantNamespace},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(aitenant, ns).Build()
	ref, err := ForNamespace(ctx, c, defaultNS, tenantreconcile.DefaultAITenantNamespace, defaultNS, "maas-default-gateway", "openshift-ingress", true)
	if err != nil {
		t.Fatalf("ForNamespace() error = %v", err)
	}
	if ref.Name != "maas-default-gateway" || ref.Namespace != "openshift-ingress" {
		t.Fatalf("ForNamespace() = %#v, want controller flag fallback", ref)
	}
}

func TestForNamespace_LegacyTenantGatewayRef(t *testing.T) {
	ctx := context.Background()
	const tenantNS = "team-a-maas"

	legacy := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNS},
		Spec: maasv1alpha1.TenantSpec{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "team-a-gateway",
			Namespace: "team-a-gateway-ns",
		}},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS}}

	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(legacy, ns).Build()
	ref, err := ForNamespace(ctx, c, tenantNS, "ai-tenants", "models-as-a-service", "fallback-gw", "fallback-ns", true)
	if err != nil {
		t.Fatalf("ForNamespace() error = %v", err)
	}
	if ref.Name != "team-a-gateway" || ref.Namespace != "team-a-gateway-ns" {
		t.Fatalf("ForNamespace() = %#v, want legacy Tenant gateway", ref)
	}
}
