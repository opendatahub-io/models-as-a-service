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

	"github.com/go-logr/logr"
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
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

// newMaasModelRef is a helper function to create a MaasModelRef resource.
func newMaaSModelRef(name, ns, kind, refName, refNS string) *maasv1alpha1.MaaSModelRef {
	m := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: kind,
				Name: refName,
			},
		},
	}
	if refNS != "" {
		m.Spec.ModelRef.Namespace = refNS
	}
	return m
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
		Build()
	return &MaaSModelRefReconciler{Client: c, Scheme: scheme}, c
}

// assertReadyCondition checks that the conditions slice contains a Ready condition
// with the expected status and reason.
func assertReadyCondition(t *testing.T, conditions []metav1.Condition, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	for _, c := range conditions {
		if c.Type == "Ready" {
			if c.Status != wantStatus {
				t.Errorf("Ready condition Status = %q, want %q", c.Status, wantStatus)
			}
			if c.Reason != wantReason {
				t.Errorf("Ready condition Reason = %q, want %q", c.Reason, wantReason)
			}
			return
		}
	}
	t.Error("Ready condition not found in status conditions")
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

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model).
				WithStatusSubresource(model).
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
	model := newMaaSModelRef(modelName, ns, "LLMInferenceService", llmisvcName, "")
	r, c := newTestReconciler(model, route, llmisvc)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is not-ready -> model enters Pending ---

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (llmisvc not-ready): %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}
	if got.Status.Phase != "Pending" {
		t.Fatalf("after first reconcile: Phase = %q, want Pending", got.Status.Phase)
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
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: watchReq.NamespacedName}); err != nil {
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
func TestMaaSModelReconciler_LLMISvcReadyToNotReady_ModelBecomesPending(t *testing.T) {
	ctx := context.Background()
	const (
		modelName   = "test-model"
		llmisvcName = "test-llmisvc"
		ns          = "default"
	)

	route := newLLMISvcRoute(llmisvcName, ns)
	llmisvc := newLLMISvc(llmisvcName, ns, corev1.ConditionTrue)
	model := newMaaSModelRef(modelName, ns, "LLMInferenceService", llmisvcName, "")
	r, c := newTestReconciler(model, route, llmisvc)
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

	// --- Phase 2: KServe marks the llmisvc not-ready -> model should become Pending ---

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
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: watchReq.NamespacedName}); err != nil {
			t.Fatalf("Reconcile (triggered by LLMInferenceService watch): %v", err)
		}
	}

	final := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get MaaSModelRef after llmisvc became not-ready: %v", err)
	}
	if final.Status.Phase != "Pending" {
		t.Errorf("after llmisvc became not-ready: Phase = %q, want Pending", final.Status.Phase)
	}
	assertReadyCondition(t, final.Status.Conditions, metav1.ConditionFalse, "BackendNotReady")
}

// TestMapLLMISvcToMaaSModels verifies edge cases for the mapper function that maps
// LLMInferenceService changes to the MaaSModels that reference them.
func TestMapLLMISvcToMaaSModels(t *testing.T) {
	t.Run("different_kind_not_enqueued", func(t *testing.T) {
		svc := newLLMISvc("my-svc", "default")
		model := newMaaSModelRef("ext-model", "default", "ExternalModel", "my-svc", "")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for ExternalModel kind, got %d: %v", len(requests), requests)
		}
	})

	t.Run("different_name_not_enqueued", func(t *testing.T) {
		svc := newLLMISvc("svc-beta", "default")
		model := newMaaSModelRef("my-model", "default", "LLMInferenceService", "svc-alpha", "")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for different name, got %d: %v", len(requests), requests)
		}
	})

	t.Run("cross_namespace_match", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "ns-b")
		model := newMaaSModelRef("cross-ns-model", "ns-a", "LLMInferenceService", "shared-svc", "ns-b")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request for cross-namespace match, got %d: %v", len(requests), requests)
		}
		if requests[0].Name != "cross-ns-model" || requests[0].Namespace != "ns-a" {
			t.Errorf("request = %v, want {Name: cross-ns-model, Namespace: ns-a}", requests[0].NamespacedName)
		}
	})

	t.Run("cross_namespace_no_match", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "ns-c")
		model := newMaaSModelRef("cross-ns-model", "ns-a", "LLMInferenceService", "shared-svc", "ns-b")
		r, _ := newTestReconciler(model, svc)
		requests := r.mapLLMISvcToMaaSModelRefs(context.Background(), svc)
		if len(requests) != 0 {
			t.Errorf("expected no requests for namespace mismatch, got %d: %v", len(requests), requests)
		}
	})

	t.Run("multiple_models_same_llmisvc", func(t *testing.T) {
		svc := newLLMISvc("shared-svc", "default")
		model1 := newMaaSModelRef("model-1", "default", "LLMInferenceService", "shared-svc", "")
		model2 := newMaaSModelRef("model-2", "default", "LLMInferenceService", "shared-svc", "")
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
					if err := r.deleteGeneratedPoliciesByLabel(context.Background(), logr.Discard(), modelName, res.kind, res.group, res.version); err != nil {
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
