/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const testAITenantNamespace = "ai-tenants"

// testRoute creates an HTTPRoute with the given gateway parentRef.
func testRoute(name, ns, gatewayName, gatewayNamespace string) *gatewayapiv1.HTTPRoute {
	gwNS := gatewayapiv1.Namespace(gatewayNamespace)
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{{
					Name:      gatewayapiv1.ObjectName(gatewayName),
					Namespace: &gwNS,
				}},
			},
		},
	}
}

func TestResolveGatewayRef_WithTenantRef(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redteam",
			Namespace: testAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "redteam-gateway",
				Namespace: "openshift-ingress",
			},
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "redteam",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()
	// Patch status since fake client WithObjects doesn't set status
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "redteam-gateway",
			Namespace: "openshift-ingress",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	route := testRoute("test-route", "model-ns", "redteam-gateway", "openshift-ingress")
	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "redteam-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "redteam-gateway")
	}
	if ref.Namespace != "openshift-ingress" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "openshift-ingress")
	}
	if model.Status.ResolvedTenantRef != "redteam" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "redteam")
	}
}

func TestResolveGatewayRef_WithTenantRef_NotFound(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "nonexistent",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	route := testRoute("test-route", "model-ns", testGatewayName, testGatewayNamespace)
	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for nonexistent AITenant, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'not found'", err)
	}
}

func TestResolveGatewayRef_WithTenantRef_NoGatewayInStatus(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-tenant",
			Namespace: testAITenantNamespace,
		},
		Spec:   maasv1alpha1.AITenantSpec{},
		Status: maasv1alpha1.AITenantStatus{},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "empty-tenant",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	route := testRoute("test-route", "model-ns", testGatewayName, testGatewayNamespace)
	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for AITenant without gateway in status, got nil")
	}
	if !strings.Contains(err.Error(), "no gateway reference") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'no gateway reference'", err)
	}
}

func TestResolveGatewayRef_WithTenantRef_OverridesModelNamespace(t *testing.T) {
	// This test verifies the core bug fix: even when model.Namespace points
	// to a different tenant, the tenantRef-based resolution uses the correct AITenant.
	ctx := context.Background()

	// AITenant for the correct tenant
	correctTenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "correct-tenant",
			Namespace: testAITenantNamespace,
		},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "correct-gateway",
				Namespace: "correct-ns",
			},
		},
	}

	// Model is in a different namespace than the tenant it references
	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "some-other-namespace"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "correct-tenant",
		},
	}

	objs := []client.Object{correctTenant}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// Patch status
	correctTenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "correct-gateway",
			Namespace: "correct-ns",
		},
	}
	if err := c.Status().Update(ctx, correctTenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	route := testRoute("test-route", "some-other-namespace", "correct-gateway", "correct-ns")
	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	// Gateway should come from the AITenant, NOT from model.Namespace
	if ref.Name != "correct-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "correct-gateway")
	}
	if ref.Namespace != "correct-ns" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "correct-ns")
	}
	if model.Status.ResolvedTenantRef != "correct-tenant" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "correct-tenant")
	}
}

// --- Auto-resolution tests (spec.tenantRef empty) ---

func TestResolveGatewayRef_AutoResolve_MatchesAITenant(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-alpha",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "alpha-gateway",
			Namespace: "openshift-ingress",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	route := testRoute("test-route", "model-ns", "alpha-gateway", "openshift-ingress")
	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "alpha-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "alpha-gateway")
	}
	if ref.Namespace != "openshift-ingress" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "openshift-ingress")
	}
	if model.Status.ResolvedTenantRef != "team-alpha" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "team-alpha")
	}
}

func TestResolveGatewayRef_AutoResolve_NoMatchingTenant(t *testing.T) {
	ctx := context.Background()

	// AITenant with a different gateway
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-beta",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "beta-gateway",
			Namespace: "openshift-ingress",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute references a gateway that no AITenant owns
	route := testRoute("test-route", "model-ns", "unknown-gateway", "openshift-ingress")
	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error when no AITenant matches gateway, got nil")
	}
	if !strings.Contains(err.Error(), "no AITenant found") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'no AITenant found'", err)
	}
	if model.Status.ResolvedTenantRef != "" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want empty after failed resolution", model.Status.ResolvedTenantRef)
	}
}

func TestResolveGatewayRef_AutoResolve_NoParentRefs(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute with no parentRefs
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
	}
	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for HTTPRoute with no parentRefs, got nil")
	}
	if !strings.Contains(err.Error(), "no gateway parentRefs") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'no gateway parentRefs'", err)
	}
}

func TestResolveGatewayRef_AutoResolve_ClearsStaleResolvedTenantRef(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
		Status: maasv1alpha1.MaaSModelStatus{
			ResolvedTenantRef: "old-tenant",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// No matching AITenant — stale resolvedTenantRef should be cleared
	route := testRoute("test-route", "model-ns", "some-gateway", "some-ns")
	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error when no AITenant matches, got nil")
	}
	if model.Status.ResolvedTenantRef != "" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want empty (stale value not cleared)", model.Status.ResolvedTenantRef)
	}
}

func TestResolveGatewayRef_AutoResolve_MultipleParentRefs(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-gamma",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "gamma-gateway",
			Namespace: "gateway-ns",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute with multiple parentRefs — second one matches
	gwNS1 := gatewayapiv1.Namespace("other-ns")
	gwNS2 := gatewayapiv1.Namespace("gateway-ns")
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{Name: "unrelated-gw", Namespace: &gwNS1},
					{Name: "gamma-gateway", Namespace: &gwNS2},
				},
			},
		},
	}
	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "gamma-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "gamma-gateway")
	}
	if ref.Namespace != "gateway-ns" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "gateway-ns")
	}
	if model.Status.ResolvedTenantRef != "team-gamma" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "team-gamma")
	}
}

func TestResolveGatewayRef_AutoResolve_ParentRefWithoutNamespace(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "local-tenant",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// AITenant's gateway is in the route's namespace (parentRef.Namespace is nil)
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "local-gw",
			Namespace: "model-ns",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// parentRef without explicit namespace — defaults to route namespace
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{Name: "local-gw"},
				},
			},
		},
	}
	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "local-gw" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "local-gw")
	}
	if ref.Namespace != "model-ns" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "model-ns")
	}
	if model.Status.ResolvedTenantRef != "local-tenant" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "local-tenant")
	}
}

// TestResolveGatewayRef_AutoResolve_SkipsServiceParentRef verifies that
// Service parentRefs (e.g., core API group Service) are filtered out and not
// resolved as Gateways (regression test for CWE-20 fix).
func TestResolveGatewayRef_AutoResolve_SkipsServiceParentRef(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-alpha",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// Patch AITenant status
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "alpha-gateway",
			Namespace: "gateway-ns",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute with only a Service parentRef (core API group) — should NOT resolve
	serviceKind := gatewayapiv1.Kind("Service")
	coreGroup := gatewayapiv1.Group("")
	gwNS := gatewayapiv1.Namespace("model-ns")
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{Name: "my-service", Namespace: &gwNS, Kind: &serviceKind, Group: &coreGroup},
				},
			},
		},
	}

	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for Service parentRef, got nil")
	}
	if !strings.Contains(err.Error(), "no AITenant found") {
		t.Errorf("resolveGatewayRef() error = %q, want to contain 'no AITenant found'", err.Error())
	}
	if model.Status.ResolvedTenantRef != "" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want empty", model.Status.ResolvedTenantRef)
	}
}

// TestResolveGatewayRef_AutoResolve_MixedParentRefs verifies that when an
// HTTPRoute has both Gateway and Service parentRefs, only the Gateway is used
// for tenant resolution (regression test for CWE-20 fix).
func TestResolveGatewayRef_AutoResolve_MixedParentRefs(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-beta",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// Patch AITenant status
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "beta-gateway",
			Namespace: "gateway-ns",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute with both Service and Gateway parentRefs
	serviceKind := gatewayapiv1.Kind("Service")
	coreGroup := gatewayapiv1.Group("")
	gwNS1 := gatewayapiv1.Namespace("model-ns")
	gwNS2 := gatewayapiv1.Namespace("gateway-ns")
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					// Service parentRef first
					{Name: "my-service", Namespace: &gwNS1, Kind: &serviceKind, Group: &coreGroup},
					// Gateway parentRef second — should match
					{Name: "beta-gateway", Namespace: &gwNS2},
				},
			},
		},
	}

	ref, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "beta-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "beta-gateway")
	}
	if ref.Namespace != "gateway-ns" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "gateway-ns")
	}
	if model.Status.ResolvedTenantRef != "team-beta" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "team-beta")
	}
}

// TestResolveGatewayRef_AutoResolve_ServiceWithMatchingNamespace verifies
// that a Service parentRef with matching name/namespace but wrong kind is
// correctly filtered out (regression test for CWE-20 fix).
func TestResolveGatewayRef_AutoResolve_ServiceWithMatchingNamespace(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-gamma",
			Namespace: testAITenantNamespace,
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// Patch AITenant status with a Gateway name/ns that matches what the
	// Service parentRef will use
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "matching-name",
			Namespace: "matching-ns",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	// HTTPRoute with Service parentRef using the same name/namespace as the
	// AITenant's gateway — should NOT match because kind=Service
	serviceKind := gatewayapiv1.Kind("Service")
	coreGroup := gatewayapiv1.Group("")
	gwNS := gatewayapiv1.Namespace("matching-ns")
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "model-ns"},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{Name: "matching-name", Namespace: &gwNS, Kind: &serviceKind, Group: &coreGroup},
				},
			},
		},
	}

	_, err := h.r.resolveGatewayRef(ctx, logr.Discard(), model, route)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for Service parentRef with matching name/ns, got nil")
	}
	if !strings.Contains(err.Error(), "no AITenant found") {
		t.Errorf("resolveGatewayRef() error = %q, want to contain 'no AITenant found'", err.Error())
	}
	if model.Status.ResolvedTenantRef != "" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want empty", model.Status.ResolvedTenantRef)
	}
}
