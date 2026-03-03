/*
Copyright 2025.

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
	"testing"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func init() {
	utilruntime.Must(kservev1alpha1.AddToScheme(scheme))
}

func TestMaaSModelReconciler_gatewayName(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayName(); got != defaultGatewayName {
			t.Errorf("gatewayName() = %q, want %q", got, defaultGatewayName)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayName: "my-gateway"}
		if got := r.gatewayName(); got != "my-gateway" {
			t.Errorf("gatewayName() = %q, want %q", got, "my-gateway")
		}
	})
}

func TestMaaSModelReconciler_gatewayNamespace(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayNamespace(); got != defaultGatewayNamespace {
			t.Errorf("gatewayNamespace() = %q, want %q", got, defaultGatewayNamespace)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayNamespace: "my-ns"}
		if got := r.gatewayNamespace(); got != "my-ns" {
			t.Errorf("gatewayNamespace() = %q, want %q", got, "my-ns")
		}
	})
}

// TestMaaSModelReconciler_LLMISvcReadyTransition_ModelBecomesReady verifies that when
// a backing LLMInferenceService transitions from not-ready to ready, the MaaSModel
// is automatically re-reconciled and moves from Pending to Ready.
func TestMaaSModelReconciler_LLMISvcReadyTransition_ModelBecomesReady(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	// HTTPRoute created by KServe for the LLMInferenceService.
	// A hostname is included so that GetModelEndpoint can build an endpoint URL
	// without needing a Gateway object in the fake client.
	gwNS := gatewayapiv1.Namespace(defaultGatewayNamespace)
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-llmisvc-route",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":      llmisvcName,
				"app.kubernetes.io/component": "llminferenceservice-router",
				"app.kubernetes.io/part-of":   "llminferenceservice",
			},
		},
		Spec: gatewayapiv1.HTTPRouteSpec{
			Hostnames: []gatewayapiv1.Hostname{"model.example.com"},
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{{
					Name:      gatewayapiv1.ObjectName(defaultGatewayName),
					Namespace: &gwNS,
				}},
			},
		},
	}

	// LLMInferenceService, initially not-ready.
	llmisvc := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: llmisvcName, Namespace: ns},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			Status: duckv1.Status{
				Conditions: duckv1.Conditions{{Type: "Ready", Status: corev1.ConditionFalse}},
			},
		},
	}

	model := &maasv1alpha1.MaaSModel{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: "LLMInferenceService",
				Name: llmisvcName,
			},
		},
	}

	// LLMInferenceService is not registered as a status subresource so that plain
	// Update() can set its status, mirroring KServe's own controller behaviour.
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(model, route, llmisvc).
		WithStatusSubresource(&maasv1alpha1.MaaSModel{}).
		WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
		Build()

	r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is not-ready -> model enters Pending ---

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (llmisvc not-ready): %v", err)
	}
	got := &maasv1alpha1.MaaSModel{}
	if err := fakeClient.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Pending" {
		t.Fatalf("after first reconcile: Phase = %q, want Pending", got.Status.Phase)
	}

	// --- Phase 2: KServe marks the llmisvc ready -> model should become Ready ---

	currentLLMISvc := &kservev1alpha1.LLMInferenceService{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: llmisvcName, Namespace: ns}, currentLLMISvc); err != nil {
		t.Fatalf("Get llmisvc: %v", err)
	}
	currentLLMISvc.Status.Conditions = duckv1.Conditions{{Type: "Ready", Status: corev1.ConditionTrue}}
	if err := fakeClient.Update(ctx, currentLLMISvc); err != nil {
		t.Fatalf("Update llmisvc to ready: %v", err)
	}

	// Simulate what a watch on LLMInferenceService should do: map the changed
	// LLMInferenceService back to its referencing MaaSModel(s) and enqueue a
	// reconcile request for each. We call mapLLMISvcToMaaSModels to obtain the
	// requests, then reconcile each one.
	requests := r.mapLLMISvcToMaaSModels(ctx, currentLLMISvc)
	if len(requests) == 0 {
		t.Fatal("mapLLMISvcToMaaSModels returned no requests; the MaaSModel referencing this LLMInferenceService should have been enqueued")
	}
	for _, watchReq := range requests {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: watchReq.NamespacedName}); err != nil {
			t.Fatalf("Reconcile (triggered by LLMInferenceService watch): %v", err)
		}
	}

	final := &maasv1alpha1.MaaSModel{}
	if err := fakeClient.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get MaaSModel after llmisvc became ready: %v", err)
	}
	if final.Status.Phase != "Ready" {
		t.Errorf("after llmisvc became ready: Phase = %q, want Ready", final.Status.Phase)
	}
}

// TestMaaSModelReconciler_LLMISvcReadyToNotReady_ModelBecomesPending verifies that when
// a backing LLMInferenceService transitions from ready to not-ready, the MaaSModel
// is automatically re-reconciled and moves from Ready back to Pending.
func TestMaaSModelReconciler_LLMISvcReadyToNotReady_ModelBecomesPending(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	// HTTPRoute created by KServe for the LLMInferenceService.
	gwNS := gatewayapiv1.Namespace(defaultGatewayNamespace)
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-llmisvc-route",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":      llmisvcName,
				"app.kubernetes.io/component": "llminferenceservice-router",
				"app.kubernetes.io/part-of":   "llminferenceservice",
			},
		},
		Spec: gatewayapiv1.HTTPRouteSpec{
			Hostnames: []gatewayapiv1.Hostname{"model.example.com"},
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{{
					Name:      gatewayapiv1.ObjectName(defaultGatewayName),
					Namespace: &gwNS,
				}},
			},
		},
	}

	// LLMInferenceService, initially ready.
	llmisvc := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: llmisvcName, Namespace: ns},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			Status: duckv1.Status{
				Conditions: duckv1.Conditions{{Type: "Ready", Status: corev1.ConditionTrue}},
			},
		},
	}

	model := &maasv1alpha1.MaaSModel{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: "LLMInferenceService",
				Name: llmisvcName,
			},
		},
	}

	// LLMInferenceService is not registered as a status subresource so that plain
	// Update() can set its status, mirroring KServe's own controller behaviour.
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(model, route, llmisvc).
		WithStatusSubresource(&maasv1alpha1.MaaSModel{}).
		WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
		Build()

	r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is ready -> model enters Ready ---

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (llmisvc ready): %v", err)
	}
	got := &maasv1alpha1.MaaSModel{}
	if err := fakeClient.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Fatalf("after first reconcile: Phase = %q, want Ready", got.Status.Phase)
	}

	// --- Phase 2: KServe marks the llmisvc not-ready -> model should become Pending ---

	currentLLMISvc := &kservev1alpha1.LLMInferenceService{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: llmisvcName, Namespace: ns}, currentLLMISvc); err != nil {
		t.Fatalf("Get llmisvc: %v", err)
	}
	currentLLMISvc.Status.Conditions = duckv1.Conditions{{Type: "Ready", Status: corev1.ConditionFalse}}
	if err := fakeClient.Update(ctx, currentLLMISvc); err != nil {
		t.Fatalf("Update llmisvc to not-ready: %v", err)
	}

	// Simulate the watch: map the changed LLMInferenceService back to its
	// referencing MaaSModel(s) and enqueue a reconcile request for each.
	requests := r.mapLLMISvcToMaaSModels(ctx, currentLLMISvc)
	if len(requests) == 0 {
		t.Fatal("mapLLMISvcToMaaSModels returned no requests; the MaaSModel referencing this LLMInferenceService should have been enqueued")
	}
	for _, watchReq := range requests {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: watchReq.NamespacedName}); err != nil {
			t.Fatalf("Reconcile (triggered by LLMInferenceService watch): %v", err)
		}
	}

	final := &maasv1alpha1.MaaSModel{}
	if err := fakeClient.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get MaaSModel after llmisvc became not-ready: %v", err)
	}
	if final.Status.Phase != "Pending" {
		t.Errorf("after llmisvc became not-ready: Phase = %q, want Pending", final.Status.Phase)
	}
}

// TestMapLLMISvcToMaaSModels verifies edge cases for the mapper function that maps
// LLMInferenceService changes to the MaaSModels that reference them.
func TestMapLLMISvcToMaaSModels(t *testing.T) {
	t.Run("different_kind_not_enqueued", func(t *testing.T) {
		ctx := context.Background()

		// MaaSModel references an ExternalModel, not an LLMInferenceService.
		model := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "ext-model", Namespace: "default"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind: "ExternalModel",
					Name: "my-svc",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for ExternalModel kind, got %d: %v", len(requests), requests)
		}
	})

	t.Run("different_name_not_enqueued", func(t *testing.T) {
		ctx := context.Background()

		// MaaSModel references a different LLMInferenceService name.
		model := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "default"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind: "LLMInferenceService",
					Name: "svc-alpha",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-beta", Namespace: "default"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for different name, got %d: %v", len(requests), requests)
		}
	})

	t.Run("cross_namespace_match", func(t *testing.T) {
		ctx := context.Background()

		// MaaSModel in ns-a references an LLMInferenceService in ns-b.
		model := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "cross-ns-model", Namespace: "ns-a"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind:      "LLMInferenceService",
					Name:      "shared-svc",
					Namespace: "ns-b",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-svc", Namespace: "ns-b"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request for cross-namespace match, got %d: %v", len(requests), requests)
		}
		if requests[0].Name != "cross-ns-model" || requests[0].Namespace != "ns-a" {
			t.Errorf("request = %v, want {Name: cross-ns-model, Namespace: ns-a}", requests[0].NamespacedName)
		}
	})

	t.Run("cross_namespace_no_match", func(t *testing.T) {
		ctx := context.Background()

		// MaaSModel in ns-a references an LLMInferenceService in ns-b,
		// but the actual LLMInferenceService is in ns-c.
		model := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "cross-ns-model", Namespace: "ns-a"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind:      "LLMInferenceService",
					Name:      "shared-svc",
					Namespace: "ns-b",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-svc", Namespace: "ns-c"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for namespace mismatch, got %d: %v", len(requests), requests)
		}
	})

	t.Run("llmisvc_alias_enqueued", func(t *testing.T) {
		ctx := context.Background()

		// MaaSModel uses the backwards-compat "llmisvc" kind alias.
		model := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "alias-model", Namespace: "default"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind: "llmisvc",
					Name: "my-svc",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "my-svc", Namespace: "default"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request for llmisvc alias kind, got %d: %v", len(requests), requests)
		}
		if requests[0].Name != "alias-model" {
			t.Errorf("request name = %q, want alias-model", requests[0].Name)
		}
	})

	t.Run("multiple_models_same_llmisvc", func(t *testing.T) {
		ctx := context.Background()

		// Two MaaSModels both reference the same LLMInferenceService.
		model1 := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "model-1", Namespace: "default"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind: "LLMInferenceService",
					Name: "shared-svc",
				},
			},
		}
		model2 := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "model-2", Namespace: "default"},
			Spec: maasv1alpha1.MaaSModelSpec{
				ModelRef: maasv1alpha1.ModelReference{
					Kind: "LLMInferenceService",
					Name: "shared-svc",
				},
			},
		}

		llmisvc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-svc", Namespace: "default"},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(model1, model2, llmisvc).
			WithIndex(&maasv1alpha1.MaaSModel{}, modelRefNameIndex, modelRefNameIndexer).
			Build()

		r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
		requests := r.mapLLMISvcToMaaSModels(ctx, llmisvc)
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests for two models referencing same llmisvc, got %d: %v", len(requests), requests)
		}

		// Verify both model names are present in the requests (order may vary).
		names := map[string]bool{}
		for _, req := range requests {
			names[req.Name] = true
		}
		if !names["model-1"] {
			t.Errorf("expected model-1 in requests, got %v", requests)
		}
		if !names["model-2"] {
			t.Errorf("expected model-2 in requests, got %v", requests)
		}
	})
}

func TestLlmisvcReadyChangedPredicate(t *testing.T) {
	p := llmisvcReadyChangedPredicate{}

	newLLMISvc := func(readyStatus corev1.ConditionStatus) *kservev1alpha1.LLMInferenceService {
		return &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Status: kservev1alpha1.LLMInferenceServiceStatus{
				Status: duckv1.Status{
					Conditions: duckv1.Conditions{{Type: "Ready", Status: readyStatus}},
				},
			},
		}
	}

	t.Run("ready_changed_true_to_false", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc(corev1.ConditionTrue),
			ObjectNew: newLLMISvc(corev1.ConditionFalse),
		}
		if !p.Update(e) {
			t.Error("expected Update to return true when Ready changes from True to False")
		}
	})

	t.Run("ready_changed_false_to_true", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc(corev1.ConditionFalse),
			ObjectNew: newLLMISvc(corev1.ConditionTrue),
		}
		if !p.Update(e) {
			t.Error("expected Update to return true when Ready changes from False to True")
		}
	})

	t.Run("ready_unchanged_true", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc(corev1.ConditionTrue),
			ObjectNew: newLLMISvc(corev1.ConditionTrue),
		}
		if p.Update(e) {
			t.Error("expected Update to return false when Ready status is unchanged (True)")
		}
	})

	t.Run("ready_unchanged_false", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc(corev1.ConditionFalse),
			ObjectNew: newLLMISvc(corev1.ConditionFalse),
		}
		if p.Update(e) {
			t.Error("expected Update to return false when Ready status is unchanged (False)")
		}
	})

	t.Run("no_ready_condition", func(t *testing.T) {
		noConditions := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		}
		e := event.UpdateEvent{ObjectOld: noConditions, ObjectNew: noConditions}
		if p.Update(e) {
			t.Error("expected Update to return false when neither object has a Ready condition")
		}
	})

	t.Run("non_llmisvc_passes_through", func(t *testing.T) {
		other := &maasv1alpha1.MaaSModel{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		}
		e := event.UpdateEvent{ObjectOld: other, ObjectNew: other}
		if !p.Update(e) {
			t.Error("expected Update to return true for non-LLMInferenceService objects")
		}
	})
}
