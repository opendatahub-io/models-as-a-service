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
		Build()

	r := &MaaSModelReconciler{Client: fakeClient, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	// --- Phase 1: reconcile while llmisvc is not-ready → model enters Pending ---

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

	// --- Phase 2: KServe marks the llmisvc ready → model should become Ready ---

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
