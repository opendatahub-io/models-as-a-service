package maas

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func TestSharedDefaultGatewayCatalogConvergesAcrossPolicyNamespaces(t *testing.T) {
	const defaultNamespace = "models-as-a-service"
	const contributorNamespace = "discovered-policies"
	tenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.DefaultAITenantName, Namespace: tenantreconcile.DefaultAITenantNamespace},
		Status:     maasv1alpha1.AITenantStatus{TenantNamespace: defaultNamespace},
	}
	config := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: defaultNamespace,
			Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      tenant.Name,
				tenantreconcile.AnnotationAITenantNamespace: tenant.Namespace,
			},
		},
	}
	contributor := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: config.Name, Namespace: contributorNamespace},
	}
	contributorNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: contributorNamespace, Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "true"}}}
	contributorPolicy := &maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: contributorNamespace}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, config, contributor, contributorNS, contributorPolicy).Build()
	r := &MaaSAuthPolicyReconciler{
		Client: kubeClient, TenantNamespace: defaultNamespace,
		GatewayNamespace: "openshift-ingress", GatewayName: "maas-default-gateway",
		InfraNamespace:                  "odh-ai-gateway-infra",
		TenantNamespaceDiscoveryEnabled: true,
	}
	ctx := context.Background()
	for i, namespace := range []string{defaultNamespace, contributorNamespace, defaultNamespace, contributorNamespace} {
		changed, err := r.reconcileGatewayAuthPolicy(ctx, logr.Discard(), nil, false, namespace, "", r.GatewayNamespace, r.GatewayName)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 0; changed != want {
			t.Fatalf("reconcile %d from namespace %q: changed=%v, want %v; shared catalog must not oscillate", i, namespace, changed, want)
		}
	}
}

func TestDefaultGatewaySubscriptionHeadersIncludeContributors(t *testing.T) {
	for _, defaultNamespace := range []string{"models-as-a-service", "custom-default-tenant"} {
		t.Run(defaultNamespace, func(t *testing.T) {
			owner := subscriptionWithHeaders(t, defaultNamespace, `{"x-grid-owner":["configured"]}`)
			contributor := subscriptionWithHeaders(t, "discovered-policies", `{"x-grid-contributor":["other"]}`)
			ownerConfig := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: defaultNamespace,
			}}
			contributorConfig := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: contributor.Namespace,
			}}
			contributorNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: contributor.Namespace, Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "true"}}}
			unrelated := subscriptionWithHeaders(t, "unlabeled", `{"x-grid-unrelated":["excluded"]}`)
			dedicated := subscriptionWithHeaders(t, "dedicated", `{"x-grid-dedicated":["excluded"]}`)
			dedicatedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dedicated.Namespace, Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "blue"}}}
			dedicatedConfig := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: ownerConfig.Name, Namespace: dedicated.Namespace, Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true", tenantreconcile.LabelTenantName: "blue",
			}}}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, contributor, ownerConfig, contributorConfig, contributorNS, unrelated, dedicated, dedicatedNS, dedicatedConfig).Build()
			r := &MaaSAuthPolicyReconciler{
				Client: kubeClient, TenantNamespace: defaultNamespace,
				GatewayNamespace: "openshift-ingress", GatewayName: "maas-default-gateway",
				TenantNamespaceDiscoveryEnabled: true,
			}
			// Unowned tenant configurations exercise the legacy nil
			// guardrail catalog, independently of the AITenant catalog above.
			for i, namespace := range []string{defaultNamespace, "discovered-policies", defaultNamespace} {
				changed, err := r.reconcileGatewayAuthPolicy(context.Background(), logr.Discard(), nil, false, namespace, "", r.GatewayNamespace, r.GatewayName)
				if err != nil {
					t.Fatal(err)
				}
				if changed != (i == 0) {
					t.Fatalf("catalog does not converge: reconcile %d changed=%v", i, changed)
				}
			}
			policy := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
			headers := nestedMapRequired(t, policy, "spec", "defaults", "rules", "response", "success", "headers")
			if _, ok := headers["x-grid-owner"]; !ok {
				t.Fatal("contributor removed the serving tenant's subscription header")
			}
			if _, ok := headers["x-grid-contributor"]; !ok {
				t.Fatal("shared gateway dropped a discovered subscription's header")
			}
			for _, excluded := range []string{"x-grid-unrelated", "x-grid-dedicated"} {
				if _, ok := headers[excluded]; ok {
					t.Fatalf("shared gateway imported excluded catalog %s", excluded)
				}
			}
			if err := kubeClient.Delete(context.Background(), contributor); err != nil {
				t.Fatal(err)
			}
			if _, err := r.reconcileGatewayAuthPolicy(context.Background(), logr.Discard(), nil, false, defaultNamespace, "", r.GatewayNamespace, r.GatewayName); err != nil {
				t.Fatal(err)
			}
			updated := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
			updatedHeaders := nestedMapRequired(t, updated, "spec", "defaults", "rules", "response", "success", "headers")
			if _, ok := updatedHeaders["x-grid-contributor"]; ok {
				t.Fatal("deleted subscription's header remained in shared catalog")
			}
		})
	}
}

func TestSharedGatewayReconcileUsesOwnerOIDC(t *testing.T) {
	const ownerNS, contributorNS = "models-as-a-service", "discovered"
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ownerNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: contributorNS, Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "true"}}},
	}
	for _, namespace := range []string{ownerNS, contributorNS} {
		objects = append(objects, &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: namespace},
			Spec: maasv1alpha1.TenantSpec{ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://" + namespace + ".example.test", ClientID: namespace, TTL: 300,
			}},
		}, newMaaSAuthPolicy("access", namespace, "team", maasv1alpha1.ModelRef{Name: "missing", Namespace: "models"}))
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(testRESTMapper()).WithObjects(objects...).WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).Build()
	r := &MaaSAuthPolicyReconciler{Client: kubeClient, Scheme: scheme, TenantNamespace: ownerNS, TenantNamespaceDiscoveryEnabled: true, GatewayNamespace: "ingress", GatewayName: "default", InfraNamespace: "infra"}
	var first map[string]any
	for _, namespace := range []string{ownerNS, contributorNS, ownerNS, contributorNS} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "access", Namespace: namespace}}); err != nil {
			t.Fatal(err)
		}
		policy := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
		spec := nestedMapRequired(t, policy, "spec")
		if first == nil {
			first = spec
		} else if !reflect.DeepEqual(first, spec) {
			t.Fatal("contributor OIDC rewrote the serving tenant's shared AuthPolicy")
		}
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), ownerNS+".example.test") {
		t.Fatal("shared policy lost owner OIDC")
	}
}

func TestLegacyGatewayWithoutDefaultNamespaceUsesPolicyCatalog(t *testing.T) {
	const namespace = "legacy-policies"
	subscription := subscriptionWithHeaders(t, namespace, `{"x-grid-legacy":["configured"]}`)
	config := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace,
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(subscription, config).Build()
	r := &MaaSAuthPolicyReconciler{Client: kubeClient, GatewayNamespace: "ingress", GatewayName: "default-gateway"}
	if _, err := r.reconcileGatewayAuthPolicy(context.Background(), logr.Discard(), nil, false, namespace, "", r.GatewayNamespace, r.GatewayName); err != nil {
		t.Fatal(err)
	}
	policy := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
	headers := nestedMapRequired(t, policy, "spec", "defaults", "rules", "response", "success", "headers")
	if _, ok := headers["x-grid-legacy"]; !ok {
		t.Fatal("unconfigured legacy gateway lost its policy namespace's catalog")
	}
}

func TestGatewayGuardrailCatalogUnionKeepsValidatedChecksAndIgnoresNil(t *testing.T) {
	var objects []runtime.Object
	for _, namespace := range []string{"tenant-a", "tenant-b"} {
		objects = append(objects,
			&maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{Name: namespace, Namespace: "ai-tenants"},
				Spec:       maasv1alpha1.AITenantSpec{Guardrails: testGuardrailAttachments("privacy")},
				Status:     maasv1alpha1.AITenantStatus{TenantNamespace: namespace},
			},
			&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace,
				Labels:      map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
				Annotations: map[string]string{tenantreconcile.AnnotationAITenantName: namespace, tenantreconcile.AnnotationAITenantNamespace: "ai-tenants"},
			}},
			&maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: namespace},
				Spec:       maasv1alpha1.MaaSSubscriptionSpec{ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{Name: "model", Namespace: namespace, Unlimited: true}}},
			},
			&maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: namespace},
				Status:     maasv1alpha1.MaaSModelStatus{ResolvedTenantRef: namespace},
			},
			testGuardrailPolicy(namespace, "privacy"),
		)
	}
	objects = append(objects, &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "legacy"}})
	r := newGuardrailHeaderTestReconciler(t, objects...)
	forward, err := r.gatewayGuardrailHeaders(context.Background(), []string{"tenant-a", "legacy", "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := r.gatewayGuardrailHeaders(context.Background(), []string{"tenant-b", "tenant-a", "legacy", "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(forward) != 2 || !reflect.DeepEqual(forward, reverse) || forward[0].ID >= forward[1].ID {
		t.Fatalf("catalog union must retain both validated checks, sorted and deduplicated: %#v / %#v", forward, reverse)
	}
}

func TestContributorCatalogChangesEnqueueSharedOwnerWithoutLocalPolicy(t *testing.T) {
	owner := newMaaSAuthPolicy("access", "models-as-a-service", "team", maasv1alpha1.ModelRef{Name: "model", Namespace: "models"})
	r := &MaaSAuthPolicyReconciler{
		Client:          fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build(),
		TenantNamespace: owner.Namespace, TenantNamespaceDiscoveryEnabled: true,
	}
	subscription := subscriptionWithHeaders(t, "discovered", `{"x-grid-test":["value"]}`)
	for _, requests := range [][]ctrl.Request{
		r.mapObjectNamespaceToMaaSAuthPolicies(context.Background(), subscription),
		r.mapNamespaceToMaaSAuthPolicies(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "discovered"}}),
	} {
		if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(owner) {
			t.Fatalf("catalog change did not enqueue shared owner: %#v", requests)
		}
	}
}
