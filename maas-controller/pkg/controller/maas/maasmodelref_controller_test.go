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
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
