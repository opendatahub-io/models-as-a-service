package maas

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func catalogRecoveryObjects(t *testing.T) []client.Object {
	t.Helper()
	objects := []client.Object{}
	for _, namespace := range []string{"models-as-a-service", "valid", "broken"} {
		objects = append(objects,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "true"}}},
			subscriptionWithHeaders(t, namespace, `{"x-grid-`+namespace+`":["value"]}`),
		)
		if namespace != "broken" {
			objects = append(objects, &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace}})
		}
	}
	return objects
}

func TestGatewayCatalogExcludesInvalidContributorAndKeepsValidation(t *testing.T) {
	for _, kind := range []string{"missing-config", "missing-tenant-label", "partial-gateway", "missing-owner"} {
		t.Run(kind, func(t *testing.T) {
			objects := catalogRecoveryObjects(t)
			config := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "broken", Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"}}}
			switch kind {
			case "missing-config":
			case "missing-tenant-label":
				objects = append(objects, config)
			case "partial-gateway":
				objects = append(objects, &maasv1alpha1.Tenant{
					ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: "broken"},
					Spec:       maasv1alpha1.TenantSpec{GatewayRef: maasv1alpha1.TenantGatewayRef{Name: "incomplete"}},
				})
			case "missing-owner":
				config.Labels[tenantreconcile.LabelTenantName] = tenantreconcile.DefaultAITenantName
				config.Labels[tenantreconcile.LabelTenantNamespace] = "broken"
				config.Annotations = map[string]string{tenantreconcile.AnnotationAITenantName: "missing", tenantreconcile.AnnotationAITenantNamespace: "ai-tenants"}
				objects = append(objects, config)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &MaaSAuthPolicyReconciler{Client: kubeClient, TenantNamespace: "models-as-a-service", TenantNamespaceDiscoveryEnabled: true, GatewayNamespace: "ingress", GatewayName: "default"}
			namespaces, unresolved, err := r.gatewayCatalogNamespaces(context.Background(), logr.Discard(), r.TenantNamespace, "", r.GatewayNamespace, r.GatewayName)
			if err != nil || !unresolved || !reflect.DeepEqual(namespaces, []string{"models-as-a-service", "valid"}) {
				t.Fatalf("invalid contributor blocked or entered catalog: namespaces=%v unresolved=%v err=%v", namespaces, unresolved, err)
			}
			if _, err := r.reconcileGatewayAuthPolicy(context.Background(), logr.Discard(), nil, false, r.TenantNamespace, "", r.GatewayNamespace, r.GatewayName); err != nil {
				t.Fatal(err)
			}
			policy := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
			headers := nestedMapRequired(t, policy, "spec", "defaults", "rules", "response", "success", "headers")
			if _, found := headers["x-grid-broken"]; found {
				t.Fatal("unresolved contributor remained in the shared header catalog")
			}
			if _, found := headers["x-grid-valid"]; !found {
				t.Fatal("valid contributor was lost")
			}
			rego := nestedStringRequired(t, policy, "spec", "defaults", "rules", "authorization", "guardrail-checks-known", "opa", "rego")
			if !strings.Contains(rego, "id in []") {
				t.Fatal("excluded configuration disabled fail-closed check validation")
			}
		})
	}
}

func TestGatewayCatalogReadFailuresMustRetry(t *testing.T) {
	for _, failure := range []error{apierrors.NewTimeoutError("temporary API timeout", 1), errors.New("transport disconnected")} {
		for _, operation := range []string{"get", "list"} {
			t.Run(operation+failure.Error(), func(t *testing.T) {
				kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(catalogRecoveryObjects(t)...).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if operation == "get" && key.Namespace == "broken" {
							return failure
						}
						return c.Get(ctx, key, obj, opts...)
					},
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if operation == "list" {
							return failure
						}
						return c.List(ctx, list, opts...)
					},
				}).Build()
				r := &MaaSAuthPolicyReconciler{Client: kubeClient, TenantNamespace: "models-as-a-service", TenantNamespaceDiscoveryEnabled: true, GatewayNamespace: "ingress", GatewayName: "default"}
				_, _, err := r.gatewayCatalogNamespaces(context.Background(), logr.Discard(), r.TenantNamespace, "", r.GatewayNamespace, r.GatewayName)
				if !errors.Is(err, failure) {
					t.Fatalf("API failure was treated as invalid configuration: %v", err)
				}
			})
		}
	}
}

func TestDeletingLastOwnerPolicyPreservesContributorGuardrailValidation(t *testing.T) {
	for _, hasContributor := range []bool{false, true} {
		t.Run(map[bool]string{false: "base-reset", true: "contributor-remains"}[hasContributor], func(t *testing.T) {
			const ownerNS = "models-as-a-service"
			now := metav1.Now()
			policy := &maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "last-owner", Namespace: ownerNS, Finalizers: []string{maasAuthPolicyFinalizer}, DeletionTimestamp: &now}}
			owner := &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.DefaultAITenantName, Namespace: "ai-tenants"},
				Status: maasv1alpha1.AITenantStatus{
					TenantNamespace: ownerNS,
					GatewayRef:      maasv1alpha1.TenantGatewayRef{Name: "default", Namespace: "ingress"},
				},
			}
			config := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: ownerNS,
				Labels: map[string]string{
					tenantreconcile.LabelManagedByAITenant: "true",
					tenantreconcile.LabelTenantName:        owner.Name,
					tenantreconcile.LabelTenantNamespace:   ownerNS,
				},
				Annotations: map[string]string{
					tenantreconcile.AnnotationAITenantName:      owner.Name,
					tenantreconcile.AnnotationAITenantNamespace: owner.Namespace,
				},
			}}
			objects := []client.Object{policy, owner, config}
			if hasContributor {
				objects = append(objects,
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "contributor", Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "true"}}},
					&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: config.Name, Namespace: "contributor"}},
					&maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "contributor"}},
				)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &MaaSAuthPolicyReconciler{Client: kubeClient, TenantNamespace: ownerNS, TenantNamespaceDiscoveryEnabled: true, GatewayNamespace: "ingress", GatewayName: "default"}
			if _, err := r.reconcileGatewayAuthPolicy(context.Background(), logr.Discard(), nil, false, ownerNS, "", r.GatewayNamespace, r.GatewayName); err != nil {
				t.Fatal(err)
			}
			if _, err := r.handleDeletion(context.Background(), logr.Discard(), policy); err != nil {
				t.Fatal(err)
			}
			generated := gatewayAuthPolicy(t, kubeClient, r.GatewayNamespace, r.gatewayAuthPolicyName(r.GatewayNamespace, r.GatewayName))
			authorization := nestedMapRequired(t, generated, "spec", "defaults", "rules", "authorization")
			_, validationPresent := authorization["guardrail-checks-known"]
			if validationPresent != hasContributor {
				t.Fatalf("guardrail validation after owner deletion=%v, contributor=%v", validationPresent, hasContributor)
			}
			var final maasv1alpha1.MaaSAuthPolicy
			if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(policy), &final); err == nil && len(final.Finalizers) > 0 {
				t.Fatal("policy cleanup did not finish")
			}
		})
	}
}
