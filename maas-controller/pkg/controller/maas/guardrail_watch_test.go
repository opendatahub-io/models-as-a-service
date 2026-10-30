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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// One watch event must recover through the real controller's error backoff,
// without another AIGuardrail event or a manual second Reconcile call.
func TestGuardrailPropagation_RetriesLookupFailures(t *testing.T) {
	for _, tt := range []struct {
		name         string
		deleteEvent  bool
		failNSLookup bool
	}{
		{name: "list failure on create"},
		{name: "list failure on delete", deleteEvent: true},
		{name: "namespace failure on delete", deleteEvent: true, failNSLookup: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var lookupAttempts, propagationAttempts atomic.Int32
			lookupErr := errors.New("lookup temporarily unavailable")
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(
					&maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "maas"}},
					&maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "tenant-a"}},
					&maasv1alpha1.MaaSAuthPolicy{ObjectMeta: metav1.ObjectMeta{Name: "ignored", Namespace: "other"}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"}}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}},
				).
				WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if !tt.failNSLookup && lookupAttempts.Add(1) == 1 {
							return lookupErr
						}
						return c.List(ctx, list, opts...)
					},
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if tt.failNSLookup && key.Name == "tenant-a" && lookupAttempts.Add(1) == 1 {
							return lookupErr
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()
			r := &MaaSAuthPolicyReconciler{Client: c, TenantNamespace: "maas", TenantNamespaceDiscoveryEnabled: true}
			policyRequests := make(chan reconcile.Request, 4)
			skipNameValidation := true
			ctrl, err := controller.NewUnmanaged("guardrail-propagation-test", controller.Options{
				Logger: logr.Discard(), SkipNameValidation: &skipNameValidation,
				Reconciler: reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
					if req == guardrailPropagationRequest {
						propagationAttempts.Add(1)
						return r.Reconcile(ctx, req)
					}
					select {
					case policyRequests <- req:
					case <-ctx.Done():
					}
					return reconcile.Result{}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			watch := &singleGuardrailEventSource{deleteEvent: tt.deleteEvent}
			if err := ctrl.Watch(&guardrailPropagationSource{SyncingSource: watch, reconciler: r}); err != nil {
				t.Fatal(err)
			}
			stopped := make(chan error, 1)
			go func() { stopped <- ctrl.Start(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-stopped:
					if err != nil {
						t.Errorf("controller stopped with error: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("controller did not stop")
				}
			})
			want := map[types.NamespacedName]bool{
				{Namespace: "maas", Name: "default"}:     true,
				{Namespace: "tenant-a", Name: "managed"}: true,
			}
			for len(want) > 0 {
				select {
				case req := <-policyRequests:
					if !want[req.NamespacedName] {
						t.Fatalf("unexpected policy request: %v", req)
					}
					delete(want, req.NamespacedName)
				case <-ctx.Done():
					t.Fatalf("missing policy requests: %v", want)
				}
			}
			if propagationAttempts.Load() != 2 || !watch.synced.Load() {
				t.Fatalf("propagation attempts = %d, source synced = %v", propagationAttempts.Load(), watch.synced.Load())
			}
		})
	}
}

// The AIGuardrail is deliberately absent from the client, as it is after deletion.
type singleGuardrailEventSource struct {
	deleteEvent bool
	synced      atomic.Bool
}

func (s *singleGuardrailEventSource) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	h := handler.EnqueueRequestsFromMapFunc(mapAIGuardrailToPropagation)
	obj := &aigatewayv1alpha1.AIGuardrail{ObjectMeta: metav1.ObjectMeta{Name: "privacy", Namespace: "models"}}
	if s.deleteEvent {
		h.Delete(ctx, event.DeleteEvent{Object: obj}, queue)
	} else {
		h.Create(ctx, event.CreateEvent{Object: obj}, queue)
	}
	return nil
}

func (s *singleGuardrailEventSource) WaitForSync(context.Context) error {
	s.synced.Store(true)
	return nil
}

func TestGuardrailPropagation_DoesNotInterceptNamespacedPolicy(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	req := guardrailPropagationRequest
	req.Namespace = "maas"
	// A real policy with the reserved name follows the normal Get/NotFound path.
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("normal policy reconcile: %v", err)
	}
}
