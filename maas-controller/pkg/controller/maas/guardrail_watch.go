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
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// An empty namespace cannot identify a real, namespaced MaaSAuthPolicy.
var guardrailPropagationRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "guardrail-catalog-refresh"}}

// guardrailPropagationSource connects to the controller-owned queue before workers
// start. The embedded source retains its normal watch and cache-sync lifecycle.
type guardrailPropagationSource struct {
	source.SyncingSource
	reconciler *MaaSAuthPolicyReconciler
}

func (s *guardrailPropagationSource) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	s.reconciler.enqueueGuardrailPolicy = queue.Add
	return s.SyncingSource.Start(ctx, queue)
}

func mapAIGuardrailToPropagation(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{guardrailPropagationRequest}
}

// propagateGuardrailChange runs inside Reconcile so lookup failures are retried
// by the controller. Model attachments can reference policies across namespaces,
// so every eligible MaaSAuthPolicy must be queued, including after policy deletion.
func (r *MaaSAuthPolicyReconciler) propagateGuardrailChange(ctx context.Context) error {
	if r.enqueueGuardrailPolicy == nil {
		return errors.New("guardrail propagation queue is not connected")
	}
	policies := &maasv1alpha1.MaaSAuthPolicyList{}
	if err := r.List(ctx, policies); err != nil {
		return fmt.Errorf("list MaaSAuthPolicies for AIGuardrail change: %w", err)
	}
	allowed := make(map[string]bool)
	requests := make([]reconcile.Request, 0, len(policies.Items))
	for _, policy := range policies.Items {
		ok, found := allowed[policy.Namespace]
		if !found {
			var err error
			ok, err = tenantNamespaceAllowed(ctx, r.Client, policy.Namespace, r.TenantNamespace, r.TenantNamespaceDiscoveryEnabled)
			if err != nil {
				return fmt.Errorf("check tenant namespace for AIGuardrail change: %w", err)
			}
			allowed[policy.Namespace] = ok
		}
		if ok {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&policy)})
		}
	}
	for _, req := range requests {
		r.enqueueGuardrailPolicy(req)
	}
	return nil
}
