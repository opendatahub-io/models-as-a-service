package maas

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestSubscriptionHeaderNames(t *testing.T) {
	subscription := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "headers-subscription",
			Namespace: "models-as-a-service",
			Annotations: map[string]string{
				subscriptionHeadersAnnotation: `{"X-Grid-Geo-Fencing":["region1","region2"]}`,
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(subscription).Build()
	reconciler := &MaaSAuthPolicyReconciler{Client: client}

	names, err := reconciler.subscriptionHeaderNames(context.Background(), "models-as-a-service")
	if err != nil {
		t.Fatalf("subscriptionHeaderNames() error = %v", err)
	}
	if len(names) != 1 || names[0] != "x-grid-geo-fencing" {
		t.Fatalf("names = %#v, want lowercase x-grid-geo-fencing", names)
	}
}

func TestSubscriptionHeaderNames_ContinuesAfterInvalidAnnotation(t *testing.T) {
	valid := subscriptionWithHeaders(t, "models-as-a-service", `{"x-grid-geo-fencing":["region1"]}`)
	valid.Name = "valid-headers-subscription"
	invalid := subscriptionWithHeaders(t, "models-as-a-service", "not-json")
	invalid.Name = "invalid-headers-subscription"

	recorder := events.NewFakeRecorder(1)
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(valid, invalid).
		Build()
	reconciler := &MaaSAuthPolicyReconciler{Client: kubeClient, Recorder: recorder}

	names, err := reconciler.subscriptionHeaderNames(context.Background(), "models-as-a-service")
	if err != nil {
		t.Fatalf("subscriptionHeaderNames() error = %v", err)
	}
	if len(names) != 1 || names[0] != "x-grid-geo-fencing" {
		t.Fatalf("names = %#v, want valid subscription header only", names)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "InvalidSubscriptionHeaders") {
			t.Fatalf("event = %q, want InvalidSubscriptionHeaders warning", event)
		}
	default:
		t.Fatal("expected InvalidSubscriptionHeaders warning event")
	}
}

func TestApplySubscriptionHeaders(t *testing.T) {
	reconciler := &MaaSAuthPolicyReconciler{InfraNamespace: "maas-system"}
	spec := reconciler.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "openshift-ingress", "maas-default-gateway", nil)

	if err := applySubscriptionHeaders(spec, []string{"x-grid-geo-fencing"}); err != nil {
		t.Fatalf("applySubscriptionHeaders() error = %v", err)
	}

	expression, found, err := unstructured.NestedString(
		spec, "defaults", "rules", "response", "success", "headers", "x-grid-geo-fencing", "plain", "expression",
	)
	if err != nil || !found {
		t.Fatalf("x-grid-geo-fencing expression: found=%v err=%v", found, err)
	}
	if !strings.Contains(expression, `headers["x-grid-geo-fencing"].join(", ")`) {
		t.Fatalf("expression = %q, want subscription headers lookup and comma join", expression)
	}
}

func TestParseSubscriptionHeaders_RejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "invalid JSON", value: "{"},
		{name: "reserved header", value: `{"Authorization":["token"]}`},
		{name: "non-grid header", value: `{"x-custom-header":["value"]}`},
		{name: "top-level null", value: "null"},
		{name: "null values", value: `{"x-grid-custom-header":null}`},
		{name: "duplicate names", value: `{"X-Grid-Custom-Header":["one"],"x-grid-custom-header":["two"]}`},
		{name: "invalid header value", value: `{"x-grid-custom-header":["bad\nvalue"]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSubscriptionHeaders(test.value); err == nil {
				t.Fatalf("parseSubscriptionHeaders(%q) succeeded, want error", test.value)
			}
		})
	}
}

func TestApplySubscriptionHeaders_DerivesRequestDenyPredicate(t *testing.T) {
	reconciler := &MaaSAuthPolicyReconciler{InfraNamespace: "maas-system"}
	spec := reconciler.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "gateway-ns", "gateway", nil)

	if err := applySubscriptionHeaders(spec, []string{"x-grid-geo-fencing"}); err != nil {
		t.Fatalf("applySubscriptionHeaders() error = %v", err)
	}

	patterns, found, err := unstructured.NestedSlice(
		spec, "defaults", "rules", "authorization", "deny-client-identity-headers", "patternMatching", "patterns",
	)
	if err != nil || !found {
		t.Fatalf("identity deny patterns: found=%v err=%v", found, err)
	}
	want := `request.headers.all(name, !name.startsWith("x-grid-") || name in ["x-grid-geo-fencing"])`
	foundPredicate := false
	for _, pattern := range patterns {
		patternMap, ok := pattern.(map[string]any)
		if ok && patternMap["predicate"] == want {
			foundPredicate = true
		}
	}
	if !foundPredicate {
		t.Fatalf("identity deny patterns = %#v, want predicate %q", patterns, want)
	}

	spec = reconciler.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "gateway-ns", "gateway", nil)
	if err := applySubscriptionHeaders(spec, nil); err != nil {
		t.Fatalf("applySubscriptionHeaders() without names error = %v", err)
	}
	patterns, found, err = unstructured.NestedSlice(
		spec, "defaults", "rules", "authorization", "deny-client-identity-headers", "patternMatching", "patterns",
	)
	if err != nil || !found {
		t.Fatalf("identity deny patterns without names: found=%v err=%v", found, err)
	}
	want = `request.headers.all(name, !name.startsWith("x-grid-"))`
	foundPredicate = false
	for _, pattern := range patterns {
		patternMap, ok := pattern.(map[string]any)
		if ok && patternMap["predicate"] == want {
			foundPredicate = true
		}
	}
	if !foundPredicate {
		t.Fatalf("identity deny patterns without names = %#v, want predicate %q", patterns, want)
	}
}

func TestReconcileGatewayAuthPolicyWithHeaders_UsesTenantNamespace(t *testing.T) {
	const (
		tenantNamespace = "ai-tenant-redteam"
		tenantID        = "redteam"
		gatewayNS       = "tenant-gateway-ns"
		gatewayName     = "tenant-gateway"
	)
	subscription := subscriptionWithHeaders(t, tenantNamespace, `{"x-grid-geo-fencing":["region1","region2"]}`)
	gateway := &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: gatewayNS},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(subscription, gateway).
		Build()
	reconciler := &MaaSAuthPolicyReconciler{
		Client:           kubeClient,
		Scheme:           scheme,
		GatewayNamespace: "default-gateway-ns",
		GatewayName:      "default-gateway",
	}

	if _, err := reconciler.reconcileGatewayAuthPolicyWithHeaders(
		context.Background(), logr.Discard(), nil, false,
		tenantNamespace, tenantID, gatewayNS, gatewayName, nil,
	); err != nil {
		t.Fatalf("reconcileGatewayAuthPolicyWithHeaders() error = %v", err)
	}

	policy := gatewayAuthPolicy(t, kubeClient, gatewayNS, reconciler.gatewayAuthPolicyName(gatewayNS, gatewayName))
	successHeaders := nestedMapRequired(t, policy, "spec", "defaults", "rules", "response", "success", "headers")
	if _, found := successHeaders["x-grid-geo-fencing"]; !found {
		t.Fatalf("success headers = %#v, want x-grid-geo-fencing from tenant namespace", successHeaders)
	}
}

func TestReconcileGatewayAuthPolicyWithHeaders_RemovesStaleSubscriptionHeaders(t *testing.T) {
	const (
		tenantNamespace = "ai-tenant-redteam"
		tenantID        = "redteam"
		gatewayNS       = "tenant-gateway-ns"
		gatewayName     = "tenant-gateway"
	)
	subscription := subscriptionWithHeaders(t, tenantNamespace, `{"x-grid-geo-fencing":["region1","region2"]}`)
	gateway := &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: gatewayNS},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(subscription, gateway).
		Build()
	reconciler := &MaaSAuthPolicyReconciler{
		Client:           kubeClient,
		Scheme:           scheme,
		GatewayNamespace: "default-gateway-ns",
		GatewayName:      "default-gateway",
	}

	if _, err := reconciler.reconcileGatewayAuthPolicyWithHeaders(
		context.Background(), logr.Discard(), nil, false,
		tenantNamespace, tenantID, gatewayNS, gatewayName, nil,
	); err != nil {
		t.Fatalf("initial reconcileGatewayAuthPolicyWithHeaders() error = %v", err)
	}

	subscription.SetAnnotations(nil)
	if err := kubeClient.Update(context.Background(), subscription); err != nil {
		t.Fatalf("remove header annotation: %v", err)
	}
	if _, err := reconciler.reconcileGatewayAuthPolicyWithHeaders(
		context.Background(), logr.Discard(), nil, false,
		tenantNamespace, tenantID, gatewayNS, gatewayName, nil,
	); err != nil {
		t.Fatalf("reconcileGatewayAuthPolicyWithHeaders() after removal error = %v", err)
	}

	policy := gatewayAuthPolicy(t, kubeClient, gatewayNS, reconciler.gatewayAuthPolicyName(gatewayNS, gatewayName))
	successHeaders := nestedMapRequired(t, policy, "spec", "defaults", "rules", "response", "success", "headers")
	if _, found := successHeaders["x-grid-geo-fencing"]; found {
		t.Fatalf("success headers = %#v, want stale x-grid-geo-fencing removed", successHeaders)
	}
}

func subscriptionWithHeaders(t *testing.T, namespace, annotation string) *maasv1alpha1.MaaSSubscription {
	t.Helper()
	return &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "headers-subscription",
			Namespace:   namespace,
			Annotations: map[string]string{subscriptionHeadersAnnotation: annotation},
		},
	}
}

func gatewayAuthPolicy(t *testing.T, kubeClient client.Client, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, policy); err != nil {
		t.Fatalf("Get AuthPolicy %s/%s: %v", namespace, name, err)
	}
	return policy
}

func TestParseSubscriptionHeaders_AllowsHTTPValidHeaderValueWhitespace(t *testing.T) {
	headers, err := parseSubscriptionHeaders(`{"x-grid-custom-header":["region1","\tregion2"]}`)
	if err != nil {
		t.Fatalf("parseSubscriptionHeaders() error = %v", err)
	}
	if values := headers["x-grid-custom-header"]; len(values) != 2 || values[1] != "\tregion2" {
		t.Fatalf("x-grid-custom-header values = %#v, want horizontal tab preserved", values)
	}
}
