package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func TestEnsureAITenantNamespaceWithClientCreatesNamespace(t *testing.T) {
	clientset := clientsetfake.NewSimpleClientset()

	if err := ensureAITenantNamespaceWithClient(context.Background(), tenantreconcile.DefaultAITenantNamespace, clientset); err != nil {
		t.Fatalf("ensure AITenant namespace: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), tenantreconcile.DefaultAITenantNamespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get AITenant namespace: %v", err)
	}
	if got := ns.Labels["opendatahub.io/generated-namespace"]; got != "true" {
		t.Fatalf("generated namespace label = %q, want true", got)
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "maas-controller" {
		t.Fatalf("managed-by label = %q, want maas-controller", got)
	}
}

func managerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func TestEnsureDefaultAITenantBootstrapCreatesAITenantFromExistingTenant(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
					UID:  types.UID("cfg-default"),
				},
			},
			&maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      maasv1alpha1.TenantInstanceName,
					Namespace: "models-as-a-service",
				},
				Spec: maasv1alpha1.TenantSpec{
					GatewayRef: maasv1alpha1.TenantGatewayRef{
						Namespace: "openshift-ingress",
						Name:      "custom-default-gateway",
					},
					ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
						IssuerURL: "https://keycloak.example.com/realms/maas",
						ClientID:  "maas-client",
						TTL:       600,
					},
				},
			},
		).
		Build()

	created, err := ensureDefaultAITenantBootstrap(
		ctx,
		cl,
		"models-as-a-service",
		tenantreconcile.DefaultAITenantNamespace,
		"opendatahub",
		tenantreconcile.MaaSControllerDeploymentName,
		"maas-default-gateway",
		"openshift-ingress",
	)
	if err != nil {
		t.Fatalf("ensure default AITenant: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true")
	}

	var aitenant maasv1alpha1.AITenant
	if err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &aitenant); err != nil {
		t.Fatalf("get default AITenant: %v", err)
	}
	if aitenant.Spec.Gateway == nil || aitenant.Spec.Gateway.Name != "custom-default-gateway" {
		t.Fatalf("gateway name = %#v, want custom-default-gateway", aitenant.Spec.Gateway)
	}
	if aitenant.Spec.OIDC == nil {
		t.Fatalf("OIDC was not copied from existing Tenant")
	}
	if got := aitenant.Spec.OIDC.IssuerURL; got != "https://keycloak.example.com/realms/maas" {
		t.Fatalf("OIDC issuer = %q, want copied issuer", got)
	}
	if got := aitenant.Spec.OIDC.ClientID; got != "maas-client" {
		t.Fatalf("OIDC clientID = %q, want maas-client", got)
	}
	if got := aitenant.Spec.OIDC.TTL; got != 600 {
		t.Fatalf("OIDC ttl = %d, want 600", got)
	}
	var cfg maasv1alpha1.Config
	if err := cl.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		t.Fatalf("get Config: %v", err)
	}
	if got := cfg.Annotations[defaultAITenantBootstrappedAnnotation]; got != "true" {
		t.Fatalf("Config bootstrap annotation = %q, want true", got)
	}
}

func TestEnsureDefaultAITenantBootstrapPreservesCustomGatewayNameFromExistingTenant(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
					UID:  types.UID("cfg-default"),
				},
			},
			&maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      maasv1alpha1.TenantInstanceName,
					Namespace: "models-as-a-service",
				},
				Spec: maasv1alpha1.TenantSpec{
					GatewayRef: maasv1alpha1.TenantGatewayRef{
						Namespace: "custom-ingress",
						Name:      "custom-default-gateway",
					},
				},
			},
		).
		Build()

	created, err := ensureDefaultAITenantBootstrap(
		ctx,
		cl,
		"models-as-a-service",
		tenantreconcile.DefaultAITenantNamespace,
		"opendatahub",
		tenantreconcile.MaaSControllerDeploymentName,
		"maas-default-gateway",
		"openshift-ingress",
	)
	if err != nil {
		t.Fatalf("ensure default AITenant: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true")
	}

	var aitenant maasv1alpha1.AITenant
	if err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &aitenant); err != nil {
		t.Fatalf("get default AITenant: %v", err)
	}
	if aitenant.Spec.Gateway == nil || aitenant.Spec.Gateway.Name != "custom-default-gateway" {
		t.Fatalf("gateway name = %#v, want custom-default-gateway", aitenant.Spec.Gateway)
	}
}

func TestEnsureDefaultAITenantBootstrapNoopsWhenAITenantExistsAndMarksConfig(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
					UID:  types.UID("cfg-default"),
				},
			},
			&maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.DefaultAITenantName,
					Namespace: tenantreconcile.DefaultAITenantNamespace,
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "already-owned"},
				},
			},
		).
		Build()

	created, err := ensureDefaultAITenantBootstrap(
		ctx,
		cl,
		"models-as-a-service",
		tenantreconcile.DefaultAITenantNamespace,
		"opendatahub",
		tenantreconcile.MaaSControllerDeploymentName,
		"maas-default-gateway",
		"openshift-ingress",
	)
	if err != nil {
		t.Fatalf("ensure default AITenant: %v", err)
	}
	if created {
		t.Fatalf("created = true, want false")
	}

	var aitenant maasv1alpha1.AITenant
	if err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &aitenant); err != nil {
		t.Fatalf("get default AITenant: %v", err)
	}
	if aitenant.Spec.Gateway == nil || aitenant.Spec.Gateway.Name != "already-owned" {
		t.Fatalf("gateway name changed to %#v, want already-owned", aitenant.Spec.Gateway)
	}
	var cfg maasv1alpha1.Config
	if err := cl.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		t.Fatalf("get Config: %v", err)
	}
	if got := cfg.Annotations[defaultAITenantBootstrappedAnnotation]; got != "true" {
		t.Fatalf("Config bootstrap annotation = %q, want true", got)
	}
}

func TestEnsureDefaultAITenantBootstrapWaitsForConfigUID(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
				},
			},
		).
		Build()

	created, err := ensureDefaultAITenantBootstrap(
		ctx,
		cl,
		"models-as-a-service",
		tenantreconcile.DefaultAITenantNamespace,
		"opendatahub",
		tenantreconcile.MaaSControllerDeploymentName,
		"maas-default-gateway",
		"openshift-ingress",
	)
	if err != nil {
		t.Fatalf("ensure default AITenant: %v", err)
	}
	if created {
		t.Fatalf("created = true, want false")
	}
	if err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &maasv1alpha1.AITenant{}); err == nil {
		t.Fatalf("default AITenant was created before Config had a UID")
	}
}

func TestEnsureDefaultAITenantBootstrapDoesNotRecreateAfterBootstrapMarker(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
					UID:  types.UID("cfg-default"),
					Annotations: map[string]string{
						defaultAITenantBootstrappedAnnotation: "true",
					},
				},
			},
		).
		Build()

	created, err := ensureDefaultAITenantBootstrap(
		ctx,
		cl,
		"models-as-a-service",
		tenantreconcile.DefaultAITenantNamespace,
		"opendatahub",
		tenantreconcile.MaaSControllerDeploymentName,
		"maas-default-gateway",
		"openshift-ingress",
	)
	if err != nil {
		t.Fatalf("ensure default AITenant: %v", err)
	}
	if created {
		t.Fatalf("created = true, want false")
	}
	if err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &maasv1alpha1.AITenant{}); err == nil {
		t.Fatalf("default AITenant was recreated after bootstrap marker")
	}
}
