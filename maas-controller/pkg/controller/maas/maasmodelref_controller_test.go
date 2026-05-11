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
	"errors"
	"testing"

	"github.com/go-logr/logr"
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// fakeHandler is a test-only BackendHandler that returns preconfigured values.
type fakeHandler struct {
	endpoint string
	ready    bool
}

func (f *fakeHandler) ReconcileRoute(_ context.Context, _ logr.Logger, _ *maasv1alpha1.MaaSModelRef) error {
	return nil
}
func (f *fakeHandler) Status(_ context.Context, _ logr.Logger, _ *maasv1alpha1.MaaSModelRef) (string, bool, error) {
	return f.endpoint, f.ready, nil
}
func (f *fakeHandler) GetModelEndpoint(_ context.Context, _ logr.Logger, _ *maasv1alpha1.MaaSModelRef) (string, error) {
	return f.endpoint, nil
}
func (f *fakeHandler) CleanupOnDelete(_ context.Context, _ logr.Logger, _ *maasv1alpha1.MaaSModelRef) error {
	return nil
}

func init() {
	utilruntime.Must(kservev1alpha1.AddToScheme(scheme))
}

// --- Test helpers ---

// newMaaSModelRef is a helper function to create a MaaSModelRef resource.
func newMaaSModelRef(name, ns, kind, refName string) *maasv1alpha1.MaaSModelRef {
	return &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: kind,
				Name: refName,
			},
		},
	}
}

// newLLMISvc is a helper function to create a LLMInferenceService resource.
func newLLMISvc(name, ns string, readyStatus ...corev1.ConditionStatus) *kservev1alpha1.LLMInferenceService {
	svc := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if len(readyStatus) > 0 {
		svc.Status = kservev1alpha1.LLMInferenceServiceStatus{
			Status: duckv1.Status{
				Conditions: duckv1.Conditions{{Type: apis.ConditionReady, Status: readyStatus[0]}},
			},
		}
	}
	return svc
}

// newLLMISvcRoute is a helper function to create a HTTPRoute resource.
func newLLMISvcRoute(llmisvcName, ns string) *gatewayapiv1.HTTPRoute {
	gwNS := gatewayapiv1.Namespace(defaultGatewayNamespace)
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      llmisvcName + "-route",
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
}

// newTestReconciler creates a MaaSModelReconciler with a fake client pre-configured
// with the field index and status subresource for MaaSModelRef. LLMInferenceService is
// intentionally NOT a status subresource so that plain Update() can set its status.
func newTestReconciler(objects ...client.Object) (*MaaSModelRefReconciler, client.Client) {
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&maasv1alpha1.MaaSModelRef{}).
		WithIndex(&maasv1alpha1.MaaSModelRef{}, modelRefNameIndex, modelRefNameIndexer).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()
	return &MaaSModelRefReconciler{Client: c, Scheme: scheme}, c
}

// assertReadyCondition checks that the conditions slice contains a Ready condition
// with the expected status and reason.
func assertReadyCondition(t *testing.T, conditions []metav1.Condition, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	assertCondition(t, conditions, "Ready", wantStatus, wantReason)
}

// assertCondition checks that the conditions slice contains a condition of the given
// type with the expected status and reason.
func assertCondition(t *testing.T, conditions []metav1.Condition, condType string, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	for _, c := range conditions {
		if c.Type == condType {
			if c.Status != wantStatus {
				t.Errorf("%s condition Status = %q, want %q", condType, c.Status, wantStatus)
			}
			if c.Reason != wantReason {
				t.Errorf("%s condition Reason = %q, want %q", condType, c.Reason, wantReason)
			}
			return
		}
	}
	t.Errorf("%s condition not found in status conditions", condType)
}

// --- Tests ---

func TestMaaSModelRefReconciler_gatewayName(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelRefReconciler{}
		if got := r.gatewayName(); got != defaultGatewayName {
			t.Errorf("gatewayName() = %q, want %q", got, defaultGatewayName)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelRefReconciler{GatewayName: "my-gateway"}
		if got := r.gatewayName(); got != "my-gateway" {
			t.Errorf("gatewayName() = %q, want %q", got, "my-gateway")
		}
	})
}

func TestReconcile_EndpointOverride(t *testing.T) {
	const testKind = "_test_fake_kind"
	discoveredEndpoint := "https://discovered.example.com/model"
	overrideEndpoint := "https://override.example.com/model"

	tests := []struct {
		name             string
		endpointOverride string
		wantEndpoint     string
	}{
		{
			name:             "uses_discovered_endpoint_when_no_override",
			endpointOverride: "",
			wantEndpoint:     discoveredEndpoint,
		},
		{
			name:             "uses_override_when_set",
			endpointOverride: overrideEndpoint,
			wantEndpoint:     overrideEndpoint,
		},
	}

	// Register a fake handler kind for testing.
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: discoveredEndpoint, ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef:         maasv1alpha1.ModelReference{Kind: testKind, Name: "backend"},
					EndpointOverride: tt.endpointOverride,
				},
			}
			sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "test-model", 100)
			sub.Spec.ModelRefs[0].Namespace = "default"
			auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
				maasv1alpha1.ModelRef{Name: "test-model", Namespace: "default"})

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, sub, auth).
				WithStatusSubresource(model).
				WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
				Build()

			r := &MaaSModelRefReconciler{Client: c, Scheme: scheme}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-model", Namespace: "default"}}

			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			updated := &maasv1alpha1.MaaSModelRef{}
			if err := c.Get(context.Background(), req.NamespacedName, updated); err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if updated.Status.Endpoint != tt.wantEndpoint {
				t.Errorf("Status.Endpoint = %q, want %q", updated.Status.Endpoint, tt.wantEndpoint)
			}
			if updated.Status.Phase != "Ready" {
				t.Errorf("Status.Phase = %q, want %q", updated.Status.Phase, "Ready")
			}
		})
	}
}

func TestMaaSModelRefReconciler_gatewayNamespace(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelRefReconciler{}
		if got := r.gatewayNamespace(); got != defaultGatewayNamespace {
			t.Errorf("gatewayNamespace() = %q, want %q", got, defaultGatewayNamespace)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelRefReconciler{GatewayNamespace: "my-ns"}
		if got := r.gatewayNamespace(); got != "my-ns" {
			t.Errorf("gatewayNamespace() = %q, want %q", got, "my-ns")
		}
	})
}

// TestMaaSModelReconciler_LLMISvcReadyTransition_ModelBecomesReady verifies that when
// a backing LLMInferenceService transitions from not-ready to ready, the MaaSModelRef
// is automatically re-reconciled and moves from Pending to Ready.
func TestMaaSModelReconciler_LLMISvcReadyTransition_ModelBecomesReady(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	route := newLLMISvcRoute(llmisvcName, ns)
	llmisvc := newLLMISvc(llmisvcName, ns, corev1.ConditionFalse)
	model := newMaaSModelRef(modelName, ns, "LLMInferenceService", llmisvcName)
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", modelName, 100)
	sub.Spec.ModelRefs[0].Namespace = ns
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: ns})
	r, c := newTestReconciler(model, route, llmisvc, sub, auth)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is not-ready -> model enters Unhealthy (governed but runtime not ready) ---

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (llmisvc not-ready): %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Unhealthy" {
		t.Fatalf("after first reconcile: Phase = %q, want Unhealthy (governed but runtime not ready)", got.Status.Phase)
	}
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")

	// --- Phase 2: KServe marks the llmisvc ready -> model should become Ready ---

	currentLLMISvc := &kservev1alpha1.LLMInferenceService{}
	if err := c.Get(ctx, types.NamespacedName{Name: llmisvcName, Namespace: ns}, currentLLMISvc); err != nil {
		t.Fatalf("Get llmisvc: %v", err)
	}
	currentLLMISvc.Status.Conditions = duckv1.Conditions{{Type: apis.ConditionReady, Status: corev1.ConditionTrue}}
	if err := c.Update(ctx, currentLLMISvc); err != nil {
		t.Fatalf("Update llmisvc to ready: %v", err)
	}

	requests := r.mapLLMISvcToMaaSModelRefs(ctx, currentLLMISvc)
	if len(requests) == 0 {
		t.Fatal("mapLLMISvcToMaaSModels returned no requests; the MaaSModelRef referencing this LLMInferenceService should have been enqueued")
	}
	for _, watchReq := range requests {
		if _, err := r.Reconcile(ctx, watchReq); err != nil {
			t.Fatalf("Reconcile (triggered by LLMInferenceService watch): %v", err)
		}
	}

	final := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get MaaSModelRef after llmisvc became ready: %v", err)
	}
	if final.Status.Phase != "Ready" {
		t.Errorf("after llmisvc became ready: Phase = %q, want Ready", final.Status.Phase)
	}
	assertReadyCondition(t, final.Status.Conditions, metav1.ConditionTrue, "Reconciled")
}

// TestMaaSModelReconciler_LLMISvcReadyToNotReady_ModelBecomesPending verifies that when
// a backing LLMInferenceService transitions from ready to not-ready, the MaaSModelRef
// is automatically re-reconciled and moves from Ready back to Pending.
func TestMaaSModelReconciler_LLMISvcReadyToNotReady_ModelBecomesUnhealthy(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	route := newLLMISvcRoute(llmisvcName, ns)
	llmisvc := newLLMISvc(llmisvcName, ns, corev1.ConditionTrue)
	model := newMaaSModelRef(modelName, ns, "LLMInferenceService", llmisvcName)
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", modelName, 100)
	sub.Spec.ModelRefs[0].Namespace = ns
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: ns})
	r, c := newTestReconciler(model, route, llmisvc, sub, auth)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is ready -> model enters Ready ---

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (llmisvc ready): %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Fatalf("after first reconcile: Phase = %q, want Ready", got.Status.Phase)
	}
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionTrue, "Reconciled")

	// --- Phase 2: KServe marks the llmisvc not-ready -> model should become Unhealthy (governed but runtime failed) ---

	currentLLMISvc := &kservev1alpha1.LLMInferenceService{}
	if err := c.Get(ctx, types.NamespacedName{Name: llmisvcName, Namespace: ns}, currentLLMISvc); err != nil {
		t.Fatalf("Get llmisvc: %v", err)
	}
	currentLLMISvc.Status.Conditions = duckv1.Conditions{{Type: apis.ConditionReady, Status: corev1.ConditionFalse}}
	if err := c.Update(ctx, currentLLMISvc); err != nil {
		t.Fatalf("Update llmisvc to not-ready: %v", err)
	}

	requests := r.mapLLMISvcToMaaSModelRefs(ctx, currentLLMISvc)
	if len(requests) == 0 {
		t.Fatal("mapLLMISvcToMaaSModels returned no requests; the MaaSModelRef referencing this LLMInferenceService should have been enqueued")
	}
	for _, watchReq := range requests {
		if _, err := r.Reconcile(ctx, watchReq); err != nil {
			t.Fatalf("Reconcile (triggered by LLMInferenceService watch): %v", err)
		}
	}

	final := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get MaaSModelRef after llmisvc became not-ready: %v", err)
	}
	if final.Status.Phase != "Unhealthy" {
		t.Errorf("after llmisvc became not-ready: Phase = %q, want Unhealthy (governed but runtime failed)", final.Status.Phase)
	}
	assertCondition(t, final.Status.Conditions, "GovernanceAttached", metav1.ConditionTrue, "GovernancePaired")
	assertCondition(t, final.Status.Conditions, "RuntimeReady", metav1.ConditionFalse, "RuntimeHealthFailure")
	assertReadyCondition(t, final.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")
}

// TestMapLLMISvcToMaaSModels verifies edge cases for the mapper function that maps
// LLMInferenceService changes to the MaaSModels that reference them.
func TestMapLLMISvcToMaaSModels(t *testing.T) {
	t.Run("different_kind_not_enqueued", func(t *testing.T) {
		svc := newLLMISvc("my-svc", "default")
		model := newMaaSModelRef("ext-model", "default", "ExternalModel", "my-svc")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for ExternalModel kind, got %d: %v", len(requests), requests)
		}
	})

	t.Run("different_name_not_enqueued", func(t *testing.T) {
		svc := newLLMISvc("svc-beta", "default")
		model := newMaaSModelRef("my-model", "default", "LLMInferenceService", "svc-alpha")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for different name, got %d: %v", len(requests), requests)
		}
	})

	t.Run("same_namespace_match", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "default")
		model := newMaaSModelRef("my-model", "default", "LLMInferenceService", "shared-svc")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request for same-namespace match, got %d: %v", len(requests), requests)
		}
		if requests[0].Name != "my-model" || requests[0].Namespace != "default" {
			t.Errorf("request = %v, want {Name: my-model, Namespace: default}", requests[0].NamespacedName)
		}
	})

	t.Run("different_namespace_not_enqueued", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "ns-b")
		model := newMaaSModelRef("my-model", "ns-a", "LLMInferenceService", "shared-svc")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for different namespace, got %d: %v", len(requests), requests)
		}
	})

	t.Run("multiple_models_same_llmisvc", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "default")
		model1 := newMaaSModelRef("model-1", "default", "LLMInferenceService", "shared-svc")
		model2 := newMaaSModelRef("model-2", "default", "LLMInferenceService", "shared-svc")
		r, _ := newTestReconciler(model1, model2, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests for two models referencing same llmisvc, got %d: %v", len(requests), requests)
		}
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

	t.Run("ready_changed_true_to_false", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc("svc", "default", corev1.ConditionTrue),
			ObjectNew: newLLMISvc("svc", "default", corev1.ConditionFalse),
		}
		if !p.Update(e) {
			t.Error("expected Update to return true when Ready changes from True to False")
		}
	})

	t.Run("ready_changed_false_to_true", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc("svc", "default", corev1.ConditionFalse),
			ObjectNew: newLLMISvc("svc", "default", corev1.ConditionTrue),
		}
		if !p.Update(e) {
			t.Error("expected Update to return true when Ready changes from False to True")
		}
	})

	t.Run("ready_unchanged_true", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc("svc", "default", corev1.ConditionTrue),
			ObjectNew: newLLMISvc("svc", "default", corev1.ConditionTrue),
		}
		if p.Update(e) {
			t.Error("expected Update to return false when Ready status is unchanged (True)")
		}
	})

	t.Run("ready_unchanged_false", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc("svc", "default", corev1.ConditionFalse),
			ObjectNew: newLLMISvc("svc", "default", corev1.ConditionFalse),
		}
		if p.Update(e) {
			t.Error("expected Update to return false when Ready status is unchanged (False)")
		}
	})

	t.Run("no_condition_to_ready", func(t *testing.T) {
		e := event.UpdateEvent{
			ObjectOld: newLLMISvc("svc", "default"),
			ObjectNew: newLLMISvc("svc", "default", corev1.ConditionTrue),
		}
		if !p.Update(e) {
			t.Error("expected Update to return true when Ready appears for first time")
		}
	})

	t.Run("no_ready_condition", func(t *testing.T) {
		noConditions := newLLMISvc("svc", "default")
		e := event.UpdateEvent{ObjectOld: noConditions, ObjectNew: noConditions}
		if p.Update(e) {
			t.Error("expected Update to return false when neither object has a Ready condition")
		}
	})

	t.Run("non_llmisvc_passes_through", func(t *testing.T) {
		other := &maasv1alpha1.MaaSModelRef{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		}
		e := event.UpdateEvent{ObjectOld: other, ObjectNew: other}
		if !p.Update(e) {
			t.Error("expected Update to return true for non-LLMInferenceService objects")
		}
	})
}

// TestMaaSModelRefReconciler_HTTPRouteRaceCondition verifies that MaaSModelRef reliably
// reaches Ready state when HTTPRoute is created after the MaaSModelRef (common during startup).
func TestMaaSModelRefReconciler_HTTPRouteRaceCondition(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	// Start with MaaSModelRef and ready LLMInferenceService, but NO HTTPRoute
	llmisvc := newLLMISvc(llmisvcName, ns, corev1.ConditionTrue)
	model := newMaaSModelRef(modelName, ns, "LLMInferenceService", llmisvcName)
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", modelName, 100)
	sub.Spec.ModelRefs[0].Namespace = ns
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: ns})
	r, c := newTestReconciler(model, llmisvc, sub, auth)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: Reconcile without HTTPRoute -> should enter Pending ---

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile (no HTTPRoute): %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue when HTTPRoute not found (watch handles it), got: %v", result)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Pending" {
		t.Errorf("Phase after first reconcile = %q, want Pending (HTTPRoute doesn't exist yet)", got.Status.Phase)
	}
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")

	// --- Phase 2: KServe creates HTTPRoute -> model should become Ready on re-reconcile ---

	route := newLLMISvcRoute(llmisvcName, ns)
	if err := c.Create(ctx, route); err != nil {
		t.Fatalf("Create HTTPRoute: %v", err)
	}

	// Reconcile again (triggered by HTTPRoute watch)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (with HTTPRoute): %v", err)
	}

	final := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get after HTTPRoute created: %v", err)
	}
	if final.Status.Phase != "Ready" {
		t.Errorf("Phase after HTTPRoute created = %q, want Ready", final.Status.Phase)
	}
	assertReadyCondition(t, final.Status.Conditions, metav1.ConditionTrue, "Reconciled")
}

// TestMaaSModelRefReconciler_DuplicateReconciliation verifies that reconciling the same
// MaaSModelRef twice does not produce a redundant status update when nothing has changed.
func TestMaaSModelRefReconciler_DuplicateReconciliation(t *testing.T) {
	const testKind = "_test_dup_recon_kind"

	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "dup-model", Namespace: "default"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: testKind, Name: "backend"},
		},
	}
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "dup-model", 100)
	sub.Spec.ModelRefs[0].Namespace = "default"
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: "dup-model", Namespace: "default"})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(model, sub, auth).
		WithStatusSubresource(model).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSModelRefReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "dup-model", Namespace: "default"}}

	// First reconcile: writes status (Ready phase).
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after reconcile #1: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Fatalf("Phase after reconcile #1 = %q, want Ready", got.Status.Phase)
	}
	rvAfterFirst := got.ResourceVersion

	// Second reconcile: nothing changed, status write should be skipped.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after reconcile #2: %v", err)
	}
	rvAfterSecond := got.ResourceVersion

	if rvAfterFirst != rvAfterSecond {
		t.Errorf("redundant status update: ResourceVersion changed from %s to %s; "+
			"second reconcile should skip the status write when nothing changed",
			rvAfterFirst, rvAfterSecond)
	}
}

// newPreexistingGeneratedPolicy builds an unstructured Kuadrant policy with the labels
// that deleteGeneratedPoliciesByLabel selects on. The name and GVK are caller-supplied
// so the same helper covers both AuthPolicy and TokenRateLimitPolicy.
func newPreexistingGeneratedPolicy(gvk schema.GroupVersionKind, name, namespace, modelName string, annotations map[string]string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(gvk)
	p.SetName(name)
	p.SetNamespace(namespace)
	p.SetLabels(map[string]string{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
	})
	p.SetAnnotations(annotations)
	return p
}

// TestMaaSModelReconciler_DeleteGeneratedPolicies_ManagedAnnotation verifies that
// deleteGeneratedPoliciesByLabel respects the opt-out annotation on both
// AuthPolicy and TokenRateLimitPolicy resources when a MaaSModelRef is deleted.
func TestMaaSModelReconciler_DeleteGeneratedPolicies_ManagedAnnotation(t *testing.T) {
	const (
		modelName  = "llm"
		namespace  = "default"
		policyName = "test-policy"
	)

	resources := []struct {
		kind    string
		group   string
		version string
	}{
		{kind: "AuthPolicy", group: "kuadrant.io", version: "v1"},
		{kind: "TokenRateLimitPolicy", group: "kuadrant.io", version: "v1alpha1"},
	}

	cases := []struct {
		name        string
		annotations map[string]string
		wantDeleted bool
	}{
		{
			name:        "annotation absent: controller deletes",
			annotations: map[string]string{},
			wantDeleted: true,
		},
		{
			name:        "opendatahub.io/managed=true: controller deletes",
			annotations: map[string]string{ManagedByODHOperator: "true"},
			wantDeleted: true,
		},
		{
			name:        "opendatahub.io/managed=false: controller must not delete",
			annotations: map[string]string{ManagedByODHOperator: "false"},
			wantDeleted: false,
		},
	}

	for _, res := range resources {
		t.Run(res.kind, func(t *testing.T) {
			gvk := schema.GroupVersionKind{Group: res.group, Version: res.version, Kind: res.kind}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					existing := newPreexistingGeneratedPolicy(gvk, policyName, namespace, modelName, tc.annotations)

					c := fake.NewClientBuilder().
						WithScheme(scheme).
						WithRESTMapper(testRESTMapper()).
						WithObjects(existing).
						Build()

					r := &MaaSModelRefReconciler{Client: c, Scheme: scheme}
					if err := r.deleteGeneratedPoliciesByLabel(context.Background(), logr.Discard(), namespace, modelName, res.kind, res.group, res.version); err != nil {
						t.Fatalf("deleteGeneratedPoliciesByLabel: unexpected error: %v", err)
					}

					got := &unstructured.Unstructured{}
					got.SetGroupVersionKind(gvk)
					err := c.Get(context.Background(), types.NamespacedName{Name: policyName, Namespace: namespace}, got)

					if tc.wantDeleted {
						if !apierrors.IsNotFound(err) {
							t.Errorf("expected %s %q to be deleted, but it still exists", res.kind, policyName)
						}
					} else {
						if err != nil {
							t.Errorf("expected %s %q to survive deletion (managed=false opt-out), but got: %v", res.kind, policyName, err)
						}
					}
				})
			}
		})
	}
}

func TestMapHTTPRouteToMaaSModelRefs(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		route        *gatewayapiv1.HTTPRoute
		models       []*maasv1alpha1.MaaSModelRef
		wantRequests int
	}{
		{
			name:  "returns all models in same namespace",
			route: newHTTPRoute("test-route", "default"),
			models: []*maasv1alpha1.MaaSModelRef{
				newMaaSModelRef("model1", "default", "LLMInferenceService", "llmisvc1"),
				newMaaSModelRef("model2", "default", "ExternalModel", "ext1"),
			},
			wantRequests: 2,
		},
		{
			name:  "ignores models in different namespace",
			route: newHTTPRoute("test-route", "default"),
			models: []*maasv1alpha1.MaaSModelRef{
				newMaaSModelRef("model1", "default", "LLMInferenceService", "llmisvc1"),
				newMaaSModelRef("model2", "other-ns", "LLMInferenceService", "llmisvc2"),
			},
			wantRequests: 1,
		},
		{
			name:         "returns empty list when no models",
			route:        newHTTPRoute("test-route", "default"),
			models:       nil,
			wantRequests: 0,
		},
		{
			name: "returns empty list when obj is not HTTPRoute",
			// Pass nil for route, but we'll create a different object type.
			// This tests that mapHTTPRouteToMaaSModelRefs properly handles non-HTTPRoute
			// objects via type assertion (returns early when obj.(*gatewayapiv1.HTTPRoute) fails).
			// We intentionally pass a MaaSModelRef to trigger the type assertion failure.
			route:        nil,
			models:       []*maasv1alpha1.MaaSModelRef{newMaaSModelRef("model1", "default", "LLMInferenceService", "llmisvc1")},
			wantRequests: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			for _, m := range tt.models {
				objects = append(objects, m)
			}

			r, _ := newTestReconciler(objects...)

			// Use either the provided route or a non-HTTPRoute object
			var obj client.Object
			if tt.route != nil {
				obj = tt.route
			} else {
				obj = &maasv1alpha1.MaaSModelRef{
					ObjectMeta: metav1.ObjectMeta{Name: "not-a-route", Namespace: "default"},
				}
			}

			requests := r.mapHTTPRouteToMaaSModelRefs(ctx, obj)

			if len(requests) != tt.wantRequests {
				t.Errorf("mapHTTPRouteToMaaSModelRefs() returned %d requests, want %d", len(requests), tt.wantRequests)
			}

			// Verify that returned requests match the models in the same namespace
			if tt.route != nil && len(requests) > 0 {
				expectedNS := tt.route.Namespace
				for _, req := range requests {
					if req.Namespace != expectedNS {
						t.Errorf("request namespace = %q, want %q", req.Namespace, expectedNS)
					}
				}
			}
		})
	}
}

func TestMapHTTPRouteToMaaSModelRefs_ListError(t *testing.T) {
	ctx := context.Background()
	route := newHTTPRoute("test-route", "default")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*maasv1alpha1.MaaSModelRefList); ok {
					return errors.New("simulated API server error")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &MaaSModelRefReconciler{Client: c, Scheme: scheme}
	requests := r.mapHTTPRouteToMaaSModelRefs(ctx, route)
	if len(requests) != 0 {
		t.Errorf("mapHTTPRouteToMaaSModelRefs() with List error returned %d requests, want 0", len(requests))
	}
}

// TestMaaSModelRefReconciler_NoSpec verifies that a legacy model ref created
// without a spec field is marked Failed without adding a finalizer.
func TestMaaSModelRefReconciler_NoSpec(t *testing.T) {
	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "no-spec", Namespace: "default"},
	}

	r, c := newTestReconciler(model)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get model: %v", err)
	}

	if len(got.Finalizers) > 0 {
		t.Errorf("expected no finalizers, got %v", got.Finalizers)
	}

	if got.Status.Phase != "Invalid" {
		t.Errorf("phase = %q, want %q", got.Status.Phase, "Invalid")
	}

	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionFalse, "InvalidSpec")
}

// --- Governance Tests ---

// TestGovernance_NoPairing verifies that a MaaSModelRef with no MaaSSubscription
// or MaaSAuthPolicy is set to Pending with GovernanceAttached: False.
func TestGovernance_NoPairing(t *testing.T) {
	const testKind = "_test_gov_no_pair"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	r, c := newTestReconciler(model)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Pending" {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionFalse, "NoPairingFound")
	assertCondition(t, got.Status.Conditions, "RuntimeReady", metav1.ConditionTrue, "RuntimeHealthy")
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")
}

// TestGovernance_ActivePairing verifies that a MaaSModelRef with both an active
// MaaSSubscription and MaaSAuthPolicy becomes Ready with GovernanceAttached: True.
func TestGovernance_ActivePairing(t *testing.T) {
	const testKind = "_test_gov_active"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "gov-model", 100)
	sub.Spec.ModelRefs[0].Namespace = "default"
	authPolicy := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: "gov-model", Namespace: "default"})

	r, c := newTestReconciler(model, sub, authPolicy)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Ready" {
		t.Errorf("Phase = %q, want Ready", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionTrue, "GovernancePaired")
	assertCondition(t, got.Status.Conditions, "RuntimeReady", metav1.ConditionTrue, "RuntimeHealthy")
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionTrue, "Reconciled")
}

// TestGovernance_PairingRemoved verifies that when a previously governed model
// loses its governance pairing, it transitions away from Ready with reason GovernanceGap.
func TestGovernance_PairingRemoved(t *testing.T) {
	const testKind = "_test_gov_removed"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	ctx := context.Background()
	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "gov-model", 100)
	sub.Spec.ModelRefs[0].Namespace = "default"
	authPolicy := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: "gov-model", Namespace: "default"})

	r, c := newTestReconciler(model, sub, authPolicy)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	// Phase 1: reconcile with governance -> Ready
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after #1: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Fatalf("Phase after #1 = %q, want Ready", got.Status.Phase)
	}

	// Phase 2: delete the subscription -> governance lost
	if err := c.Delete(ctx, sub); err != nil {
		t.Fatalf("Delete sub: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after #2: %v", err)
	}

	if got.Status.Phase != "Pending" {
		t.Errorf("Phase after governance loss = %q, want Pending", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionFalse, "GovernanceGap")
}

// TestGovernance_RuntimeFailureWithGovernance verifies that when a governed model
// has a runtime/health failure, GovernanceAttached stays True and RuntimeReady is False.
func TestGovernance_RuntimeFailureWithGovernance(t *testing.T) {
	const testKind = "_test_gov_runtime_fail"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "", ready: false}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "gov-model", 100)
	sub.Spec.ModelRefs[0].Namespace = "default"
	authPolicy := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: "gov-model", Namespace: "default"})

	r, c := newTestReconciler(model, sub, authPolicy)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Unhealthy" {
		t.Errorf("Phase = %q, want Unhealthy", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionTrue, "GovernancePaired")
	assertCondition(t, got.Status.Conditions, "RuntimeReady", metav1.ConditionFalse, "RuntimeHealthFailure")
	assertReadyCondition(t, got.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")
}

// TestGovernance_BothFailures verifies that when both governance and runtime fail,
// the status reflects both issues simultaneously.
func TestGovernance_BothFailures(t *testing.T) {
	const testKind = "_test_gov_both_fail"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "", ready: false}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	r, c := newTestReconciler(model)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Pending" {
		t.Errorf("Phase = %q, want Pending", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionFalse, "NoPairingFound")
	assertCondition(t, got.Status.Conditions, "RuntimeReady", metav1.ConditionFalse, "RuntimeHealthFailure")
}

// TestGovernance_NoAdminCRNamesInStatus verifies that no subscription or auth policy
// names, namespaces, or UIDs appear in MaaSModelRef.status.
func TestGovernance_NoAdminCRNamesInStatus(t *testing.T) {
	const testKind = "_test_gov_privacy"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-admin-subscription", Namespace: "admin-confidential-ns"},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner:     maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{Name: "gov-model", Namespace: "default", TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}}}},
		},
	}
	authPolicy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "secret-admin-policy", Namespace: "admin-confidential-ns"},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{{Name: "gov-model", Namespace: "default"}},
			Subjects:  maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
		},
	}

	r, c := newTestReconciler(model, sub, authPolicy)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Check that no admin CR names/namespaces leak into conditions
	sensitiveStrings := []string{
		"secret-admin-subscription",
		"secret-admin-policy",
		"admin-confidential-ns",
	}
	for _, cond := range got.Status.Conditions {
		for _, s := range sensitiveStrings {
			if containsString(cond.Message, s) {
				t.Errorf("condition %q message contains admin CR reference %q: %q", cond.Type, s, cond.Message)
			}
			if containsString(cond.Reason, s) {
				t.Errorf("condition %q reason contains admin CR reference %q: %q", cond.Type, s, cond.Reason)
			}
		}
	}
}

// TestGovernance_SubscriptionOnly_NotGoverned verifies that having only a
// MaaSSubscription (no MaaSAuthPolicy) does not make the model governed.
func TestGovernance_SubscriptionOnly_NotGoverned(t *testing.T) {
	const testKind = "_test_gov_sub_only"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", "gov-model", 100)
	sub.Spec.ModelRefs[0].Namespace = "default"

	r, c := newTestReconciler(model, sub)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Pending" {
		t.Errorf("Phase = %q, want Pending (sub only, no auth policy)", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionFalse, "NoPairingFound")
}

// TestGovernance_AuthPolicyOnly_NotGoverned verifies that having only a
// MaaSAuthPolicy (no MaaSSubscription) does not make the model governed.
func TestGovernance_AuthPolicyOnly_NotGoverned(t *testing.T) {
	const testKind = "_test_gov_auth_only"
	backendHandlerFactories[testKind] = func(_ *MaaSModelRefReconciler) BackendHandler {
		return &fakeHandler{endpoint: "https://model.example.com", ready: true}
	}
	defer delete(backendHandlerFactories, testKind)

	model := newMaaSModelRef("gov-model", "default", testKind, "backend")
	authPolicy := newMaaSAuthPolicy("auth1", "admin-ns", "team-a",
		maasv1alpha1.ModelRef{Name: "gov-model", Namespace: "default"})

	r, c := newTestReconciler(model, authPolicy)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gov-model", Namespace: "default"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Status.Phase != "Pending" {
		t.Errorf("Phase = %q, want Pending (auth policy only, no sub)", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "GovernanceAttached", metav1.ConditionFalse, "NoPairingFound")
}

// TestGovernance_MapSubscriptionToModels verifies that the mapper function
// correctly maps MaaSSubscription changes to the referenced MaaSModelRefs.
func TestGovernance_MapSubscriptionToModels(t *testing.T) {
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "admin-ns"},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner: maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
				{Name: "model-a", Namespace: "ns-a", TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}}},
				{Name: "model-b", Namespace: "ns-b", TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}}},
			},
		},
	}

	r := &MaaSModelRefReconciler{}
	requests := r.mapMaaSSubscriptionToMaaSModelRefs(context.Background(), sub)

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(requests), requests)
	}

	names := map[string]bool{}
	for _, req := range requests {
		names[req.Namespace+"/"+req.Name] = true
	}
	if !names["ns-a/model-a"] {
		t.Errorf("expected ns-a/model-a in requests")
	}
	if !names["ns-b/model-b"] {
		t.Errorf("expected ns-b/model-b in requests")
	}
}

// TestGovernance_MapAuthPolicyToModels verifies that the mapper function
// correctly maps MaaSAuthPolicy changes to the referenced MaaSModelRefs.
func TestGovernance_MapAuthPolicyToModels(t *testing.T) {
	policy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "auth1", Namespace: "admin-ns"},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{
				{Name: "model-a", Namespace: "ns-a"},
				{Name: "model-b", Namespace: "ns-b"},
			},
			Subjects: maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
		},
	}

	r := &MaaSModelRefReconciler{}
	requests := r.mapMaaSAuthPolicyToMaaSModelRefs(context.Background(), policy)

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(requests), requests)
	}

	names := map[string]bool{}
	for _, req := range requests {
		names[req.Namespace+"/"+req.Name] = true
	}
	if !names["ns-a/model-a"] {
		t.Errorf("expected ns-a/model-a in requests")
	}
	if !names["ns-b/model-b"] {
		t.Errorf("expected ns-b/model-b in requests")
	}
}
