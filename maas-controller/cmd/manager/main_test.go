package main

import (
	"context"
	"errors"
	"testing"

	confv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/controller/maas"
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
	utilruntime.Must(confv1.Install(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func TestFetchTLSProfileWithRetryTransientErrorFallsBackToIntermediate(t *testing.T) {
	originalDelay := tlsProfileRetryDelay
	defer func() {
		tlsProfileRetryDelay = originalDelay
	}()
	tlsProfileRetryDelay = 0

	calls := 0
	cl := controllerfake.NewClientBuilder().
		WithScheme(managerTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				calls++
				return apierrors.NewInternalError(errors.New("apiserver unavailable"))
			},
		}).
		Build()

	profile, adherence, available, err := fetchTLSProfileWithRetry(context.Background(), cl)
	if err != nil {
		t.Fatalf("fetchTLSProfileWithRetry returned error: %v", err)
	}
	if calls != tlsProfileFetchMaxRetries {
		t.Fatalf("Get calls = %d, want %d", calls, tlsProfileFetchMaxRetries)
	}
	if !available {
		t.Fatalf("available = false, want true so watcher can self-heal")
	}
	if adherence != confv1.TLSAdherencePolicyNoOpinion {
		t.Fatalf("adherence = %q, want %q", adherence, confv1.TLSAdherencePolicyNoOpinion)
	}
	if profile.MinTLSVersion != confv1.VersionTLS12 {
		t.Fatalf("MinTLSVersion = %q, want Intermediate TLS 1.2", profile.MinTLSVersion)
	}
	if len(profile.Ciphers) == 0 {
		t.Fatalf("default Intermediate profile should include TLS ciphers")
	}
}

func TestFetchTLSProfileWithRetryAPIUnavailableSkipsWatcher(t *testing.T) {
	cl := controllerfake.NewClientBuilder().
		WithScheme(managerTestScheme(t)).
		Build()

	profile, adherence, available, err := fetchTLSProfileWithRetry(context.Background(), cl)
	if err != nil {
		t.Fatalf("fetchTLSProfileWithRetry returned error: %v", err)
	}
	if available {
		t.Fatalf("available = true, want false for non-OpenShift API absence")
	}
	if adherence != confv1.TLSAdherencePolicyNoOpinion {
		t.Fatalf("adherence = %q, want %q", adherence, confv1.TLSAdherencePolicyNoOpinion)
	}
	if profile.MinTLSVersion != confv1.VersionTLS12 {
		t.Fatalf("MinTLSVersion = %q, want Intermediate TLS 1.2", profile.MinTLSVersion)
	}
}

func TestTLSProfileForAdherence(t *testing.T) {
	modern := *confv1.TLSProfiles[confv1.TLSProfileModernType]

	tests := []struct {
		name      string
		adherence confv1.TLSAdherencePolicy
		wantMin   confv1.TLSProtocolVersion
	}{
		{
			name:      "unset uses Intermediate",
			adherence: confv1.TLSAdherencePolicyNoOpinion,
			wantMin:   confv1.VersionTLS12,
		},
		{
			name:      "legacy uses Intermediate",
			adherence: confv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			wantMin:   confv1.VersionTLS12,
		},
		{
			name:      "strict uses cluster profile",
			adherence: confv1.TLSAdherencePolicyStrictAllComponents,
			wantMin:   confv1.VersionTLS13,
		},
		{
			name:      "unknown future value fails secure",
			adherence: confv1.TLSAdherencePolicy("FuturePolicy"),
			wantMin:   confv1.VersionTLS13,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tlsProfileForAdherence(modern, tt.adherence)
			if got.MinTLSVersion != tt.wantMin {
				t.Fatalf("MinTLSVersion = %q, want %q", got.MinTLSVersion, tt.wantMin)
			}
		})
	}
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
	ref := configOwnerReference(aitenant.OwnerReferences, types.UID("cfg-default"))
	if ref == nil {
		t.Fatalf("default AITenant ownerReferences = %#v, want Config/default", aitenant.OwnerReferences)
	}
	if ref.Controller != nil {
		t.Fatalf("default AITenant Config owner reference is controller ref, want non-controller")
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

func configOwnerReference(refs []metav1.OwnerReference, uid types.UID) *metav1.OwnerReference {
	for i := range refs {
		ref := &refs[i]
		if ref.APIVersion == maasv1alpha1.GroupVersion.String() &&
			ref.Kind == maasv1alpha1.ConfigKind &&
			ref.Name == maasv1alpha1.ConfigInstanceName &&
			ref.UID == uid {
			return ref
		}
	}
	return nil
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

func TestEnsureDefaultAITenantBootstrapSkipsWhenTeardownRequested(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.MaaSControllerDeploymentName,
					Namespace: "opendatahub",
					Annotations: map[string]string{
						maas.TeardownRequestedAnnotation: "true",
					},
				},
			},
			&maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.ConfigInstanceName,
					UID:  types.UID("cfg-default"),
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
	}, &aitenant); err == nil {
		t.Fatalf("default AITenant was created during teardown: %#v", aitenant)
	}
}

func TestAitenantTeardownPauseConditionPausesWhenDeploymentMissing(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	cl := controllerfake.NewClientBuilder().WithScheme(s).Build()

	deploymentKey := client.ObjectKey{Name: tenantreconcile.MaaSControllerDeploymentName, Namespace: "opendatahub"}
	pause, err := aitenantTeardownPauseCondition(cl, deploymentKey)(ctx)
	if err != nil {
		t.Fatalf("evaluate pause condition: %v", err)
	}
	if !pause {
		t.Fatalf("pause = false, want true when Deployment is missing during teardown")
	}
}

func TestAitenantTeardownPauseConditionPausesWhenTeardownRequested(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	deploymentKey := client.ObjectKey{Name: tenantreconcile.MaaSControllerDeploymentName, Namespace: "opendatahub"}
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentKey.Name,
				Namespace: deploymentKey.Namespace,
				Annotations: map[string]string{
					maas.TeardownRequestedAnnotation: "true",
				},
			},
		}).
		Build()

	pause, err := aitenantTeardownPauseCondition(cl, deploymentKey)(ctx)
	if err != nil {
		t.Fatalf("evaluate pause condition: %v", err)
	}
	if !pause {
		t.Fatalf("pause = false, want true when Deployment advertises teardown")
	}
}

func TestAitenantTeardownPauseConditionRunsWhenDeploymentIsHealthy(t *testing.T) {
	ctx := context.Background()
	s := managerTestScheme(t)
	deploymentKey := client.ObjectKey{Name: tenantreconcile.MaaSControllerDeploymentName, Namespace: "opendatahub"}
	cl := controllerfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentKey.Name,
				Namespace: deploymentKey.Namespace,
			},
		}).
		Build()

	pause, err := aitenantTeardownPauseCondition(cl, deploymentKey)(ctx)
	if err != nil {
		t.Fatalf("evaluate pause condition: %v", err)
	}
	if pause {
		t.Fatalf("pause = true, want false when Deployment does not advertise teardown")
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

func TestEnsureManagedNamespaceAddsNetworkPolicyLabelWithoutOverwritingOwnership(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-infra-ns",
			Labels: map[string]string{
				"app.kubernetes.io/part-of":    "ai-gateway",
				"app.kubernetes.io/managed-by": "ai-gateway-operator",
			},
		},
	}
	clientset := clientsetfake.NewSimpleClientset(existing)

	if err := ensureManagedNamespaceWithClient(context.Background(), "test-infra-ns", "infra", clientset); err != nil {
		t.Fatalf("ensure managed namespace: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "test-infra-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got := ns.Labels["opendatahub.io/generated-namespace"]; got != "true" {
		t.Fatalf("generated-namespace label = %q, want true", got)
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "ai-gateway-operator" {
		t.Fatalf("managed-by label was overwritten to %q, want ai-gateway-operator preserved", got)
	}
	if got := ns.Labels["app.kubernetes.io/part-of"]; got != "ai-gateway" {
		t.Fatalf("part-of label was overwritten to %q, want ai-gateway preserved", got)
	}
}

func TestEnsureManagedNamespaceNoUpdateWhenNetworkPolicyLabelPresent(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-infra-ns",
			Labels: map[string]string{
				"opendatahub.io/generated-namespace": "true",
				"app.kubernetes.io/managed-by":       "ai-gateway-operator",
				"app.kubernetes.io/part-of":          "ai-gateway",
			},
		},
	}
	clientset := clientsetfake.NewSimpleClientset(existing)

	if err := ensureManagedNamespaceWithClient(context.Background(), "test-infra-ns", "infra", clientset); err != nil {
		t.Fatalf("ensure managed namespace: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "test-infra-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "ai-gateway-operator" {
		t.Fatalf("managed-by label changed to %q, want ai-gateway-operator unchanged", got)
	}
}

func TestEnsureManagedNamespacePatchesNilLabels(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-infra-ns",
		},
	}
	clientset := clientsetfake.NewSimpleClientset(existing)

	if err := ensureManagedNamespaceWithClient(context.Background(), "test-infra-ns", "infra", clientset); err != nil {
		t.Fatalf("ensure managed namespace: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "test-infra-ns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if got := ns.Labels["opendatahub.io/generated-namespace"]; got != "true" {
		t.Fatalf("generated-namespace label = %q, want true", got)
	}
	if _, exists := ns.Labels["app.kubernetes.io/managed-by"]; exists {
		t.Fatalf("managed-by label was added to namespace not created by maas-controller")
	}
}
