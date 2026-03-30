package maas

import (
	"context"
	"testing"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newHTTPRouteForLLMISvc creates an HTTPRoute with labels expected by the LLMInferenceService route resolver
func newHTTPRouteForLLMISvc(name, ns, llmisvcName string) *gatewayapiv1.HTTPRoute {
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":      llmisvcName,
				"app.kubernetes.io/component": "llminferenceservice-router",
				"app.kubernetes.io/part-of":   "llminferenceservice",
			},
		},
	}
}

// TestMaaSSubscriptionReconciler_TRLPMergeStrategy verifies that generated
// TokenRateLimitPolicy resources use defaults.strategy: merge instead of
// top-level limits. This allows multiple TRLPs targeting the same HTTPRoute
// to coexist without conflicts (RHOAIENG-53869).
func TestMaaSSubscriptionReconciler_TRLPMergeStrategy(t *testing.T) {
	const (
		modelName     = "test-model"
		namespace     = "default"
		httpRouteName = "maas-model-" + modelName
		trlpName      = "maas-trlp-" + modelName
		maasSubName   = "sub-a"
	)

	model := newMaaSModelRef(modelName, namespace, "LLMInferenceService", modelName)
	route := newHTTPRouteForLLMISvc(httpRouteName, namespace, modelName)
	maasSub := newMaaSSubscription(maasSubName, namespace, "team-a", modelName, 1000)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasSub).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasSubName, Namespace: namespace}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	// Fetch the generated TRLP
	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: trlpName, Namespace: namespace}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy %q: %v", trlpName, err)
	}

	// Verify spec structure: defaults.strategy should exist and be "merge"
	strategy, found, err := unstructured.NestedString(trlp.Object, "spec", "defaults", "strategy")
	if err != nil {
		t.Fatalf("Failed to get spec.defaults.strategy: %v", err)
	}
	if !found {
		t.Errorf("spec.defaults.strategy not found; TRLP should use defaults.strategy: merge")
	}
	if strategy != "merge" {
		t.Errorf("spec.defaults.strategy = %q, want %q", strategy, "merge")
	}

	// Verify limits are under defaults.limits, not top-level
	_, found, err = unstructured.NestedMap(trlp.Object, "spec", "defaults", "limits")
	if err != nil {
		t.Fatalf("Failed to get spec.defaults.limits: %v", err)
	}
	if !found {
		t.Errorf("spec.defaults.limits not found; TRLP should use defaults.limits")
	}

	// Verify no top-level limits field exists
	_, found, err = unstructured.NestedMap(trlp.Object, "spec", "limits")
	if err != nil {
		t.Fatalf("Failed to check spec.limits: %v", err)
	}
	if found {
		t.Errorf("spec.limits found; TRLP should NOT use top-level limits (use defaults.limits instead)")
	}
}

// TestMaaSSubscriptionReconciler_TRLPMergeStrategyMultipleModels verifies that
// the controller creates separate TRLPs for each model, and all use strategy: merge.
// Note: In unit tests, we create separate routes per model. The E2E test validates
// the real scenario where multiple models share the same HTTPRoute.
func TestMaaSSubscriptionReconciler_TRLPMergeStrategyMultipleModels(t *testing.T) {
	const (
		namespace = "default"
		modelA    = "model-a"
		modelB    = "model-b"
		routeA    = "maas-model-model-a"
		routeB    = "maas-model-model-b"
		trlpA     = "maas-trlp-model-a"
		trlpB     = "maas-trlp-model-b"
		subA      = "sub-a"
		subB      = "sub-b"
	)

	// Create two models with separate routes (in unit tests, we can't easily
	// simulate RouteResolver finding a shared route, so we create separate routes)
	modelRefA := newMaaSModelRef(modelA, namespace, "LLMInferenceService", modelA)
	modelRefB := newMaaSModelRef(modelB, namespace, "LLMInferenceService", modelB)
	httpRouteA := newHTTPRouteForLLMISvc(routeA, namespace, modelA)
	httpRouteB := newHTTPRouteForLLMISvc(routeB, namespace, modelB)

	subscriptionA := newMaaSSubscription(subA, namespace, "team-a", modelA, 1000)
	subscriptionB := newMaaSSubscription(subB, namespace, "team-b", modelB, 5000)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelRefA, modelRefB, httpRouteA, httpRouteB, subscriptionA, subscriptionB).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}

	// Reconcile both subscriptions
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: subA, Namespace: namespace}}
	reqB := ctrl.Request{NamespacedName: types.NamespacedName{Name: subB, Namespace: namespace}}

	if _, err := r.Reconcile(context.Background(), reqA); err != nil {
		t.Fatalf("Reconcile sub-a: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reqB); err != nil {
		t.Fatalf("Reconcile sub-b: %v", err)
	}

	// Verify both TRLPs exist
	trlpObjA := &unstructured.Unstructured{}
	trlpObjA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: trlpA, Namespace: namespace}, trlpObjA); err != nil {
		t.Fatalf("Get TRLP-A: %v", err)
	}

	trlpObjB := &unstructured.Unstructured{}
	trlpObjB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: trlpB, Namespace: namespace}, trlpObjB); err != nil {
		t.Fatalf("Get TRLP-B: %v", err)
	}

	// Verify both TRLPs have strategy: merge
	for _, tc := range []struct {
		name string
		trlp *unstructured.Unstructured
	}{
		{"TRLP-A", trlpObjA},
		{"TRLP-B", trlpObjB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strategy, found, err := unstructured.NestedString(tc.trlp.Object, "spec", "defaults", "strategy")
			if err != nil {
				t.Fatalf("Failed to get spec.defaults.strategy: %v", err)
			}
			if !found {
				t.Errorf("spec.defaults.strategy not found")
			}
			if strategy != "merge" {
				t.Errorf("spec.defaults.strategy = %q, want %q", strategy, "merge")
			}

			// Verify limits are under defaults.limits
			_, found, err = unstructured.NestedMap(tc.trlp.Object, "spec", "defaults", "limits")
			if err != nil {
				t.Fatalf("Failed to get spec.defaults.limits: %v", err)
			}
			if !found {
				t.Errorf("spec.defaults.limits not found")
			}
		})
	}

	// NOTE: This unit test creates separate routes per model due to test constraints.
	// The real scenario where multiple models share the same HTTPRoute is validated
	// in E2E tests (see test/e2e/tests/test_trlp_merge_strategy.md).
}
