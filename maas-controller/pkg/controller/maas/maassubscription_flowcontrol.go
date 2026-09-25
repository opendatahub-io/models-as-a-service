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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	// inferenceObjectiveNameMaxLength keeps generated InferenceObjective names valid as
	// DNS labels and as header values.
	inferenceObjectiveNameMaxLength = 63
	inferenceObjectiveNamePrefix    = "maas"
	// inferenceObjectiveNameHashLength is the number of hex characters of the identity
	// hash kept in generated names. It is never truncated.
	inferenceObjectiveNameHashLength = 10
)

// inferenceObjectiveName returns the deterministic InferenceObjective name for a
// subscription and InferencePool. The readable tenant, subscription, and pool parts are
// truncated as needed; the hash of their full identities keeps names unique. The name
// does not depend on spec.inferencePriority, so it is stable across priority changes.
func inferenceObjectiveName(tenantName string, subscription types.NamespacedName, pool maasv1alpha1.InferencePoolReference) string {
	identity := strings.Join([]string{
		tenantName,
		subscription.Namespace, subscription.Name,
		pool.Group, pool.Kind, pool.Namespace, pool.Name,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	hash := hex.EncodeToString(sum[:])[:inferenceObjectiveNameHashLength]

	parts := []string{
		dnsLabelPart(tenantName),
		dnsLabelPart(subscription.Name),
		dnsLabelPart(pool.Name),
	}
	// Budget for the readable parts and the separators between them.
	budget := inferenceObjectiveNameMaxLength - len(inferenceObjectiveNamePrefix) - len(hash) - 2
	if len(strings.Join(parts, "-")) > budget {
		perPart := (budget - (len(parts) - 1)) / len(parts)
		for i := range parts {
			if len(parts[i]) > perPart {
				parts[i] = strings.TrimRight(parts[i][:perPart], "-")
			}
		}
	}

	nameParts := []string{inferenceObjectiveNamePrefix}
	for _, p := range parts {
		if p != "" {
			nameParts = append(nameParts, p)
		}
	}
	nameParts = append(nameParts, hash)
	return strings.Join(nameParts, "-")
}

// dnsLabelPart lowercases s and replaces characters not allowed in a DNS label with '-'.
func dnsLabelPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// flowControlTenantName returns the AITenant name for a subscription namespace. It falls
// back to the default tenant name when the namespace has no tenant config.
func flowControlTenantName(ctx context.Context, c client.Reader, namespace string) (string, error) {
	tenant, err := fetchTenantForNamespace(ctx, c, namespace)
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return tenantreconcile.TenantNameFor(nil)
		}
		return "", err
	}
	return tenant.name()
}

// resolveFlowControlStatuses reports, for each referenced model, the observed InferencePool,
// the generated InferenceObjective name, and the request-priority reconciliation state.
// A failure for one model is reported on that model and does not affect the others.
// Transient lookup errors are also returned so the caller can retry.
func (r *MaaSSubscriptionReconciler) resolveFlowControlStatuses(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription) ([]maasv1alpha1.ModelFlowControlStatus, error) {
	statuses := make([]maasv1alpha1.ModelFlowControlStatus, 0, len(subscription.Spec.ModelRefs))
	seen := make(map[string]struct{}, len(subscription.Spec.ModelRefs))
	var errs []error

	// Resolved lazily: only models with an observed pool need the tenant name.
	var tenantName string
	var tenantErr error
	tenantResolved := false

	for _, ref := range subscription.Spec.ModelRefs {
		key := ref.Namespace + "/" + ref.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		status := maasv1alpha1.ModelFlowControlStatus{Name: ref.Name, Namespace: ref.Namespace}
		pool, state, message, err := r.resolveModelInferencePool(ctx, ref.Namespace, ref.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("model %s: %w", key, err))
		}
		status.InferencePool = pool
		status.State = state
		status.Message = message

		if pool != nil {
			if !tenantResolved {
				tenantName, tenantErr = flowControlTenantName(ctx, r.Client, subscription.Namespace)
				tenantResolved = true
			}
			switch {
			case tenantErr != nil:
				status.State = maasv1alpha1.FlowControlStateFailed
				status.Message = fmt.Sprintf("failed to resolve tenant for namespace %s: %v", subscription.Namespace, tenantErr)
			case subscription.Spec.InferencePriority == nil:
				status.ObjectiveName = inferenceObjectiveName(tenantName, client.ObjectKeyFromObject(subscription), *pool)
				status.State = maasv1alpha1.FlowControlStateNotRequired
				status.Message = "spec.inferencePriority is unset; no InferenceObjective is required and the scheduler applies priority 0"
			default:
				status.ObjectiveName = inferenceObjectiveName(tenantName, client.ObjectKeyFromObject(subscription), *pool)
				status.State = maasv1alpha1.FlowControlStatePending
				status.Message = "waiting for InferenceObjective reconciliation"
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, errors.Join(errs...)
}

// resolveModelInferencePool returns the InferencePool observed for a MaaSModelRef. When no
// pool is returned, state and message explain why. A non-nil error is a transient lookup
// failure that should be retried; missing resources are reported as Pending without an error
// because the MaaSModelRef and LLMInferenceService watches re-trigger reconciliation.
func (r *MaaSSubscriptionReconciler) resolveModelInferencePool(ctx context.Context, modelNamespace, modelName string) (*maasv1alpha1.InferencePoolReference, maasv1alpha1.FlowControlState, string, error) {
	model := &maasv1alpha1.MaaSModelRef{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: modelNamespace, Name: modelName}, model); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, maasv1alpha1.FlowControlStatePending, fmt.Sprintf("MaaSModelRef %s/%s not found", modelNamespace, modelName), nil
		}
		return nil, maasv1alpha1.FlowControlStatePending, fmt.Sprintf("failed to get MaaSModelRef: %v", err),
			fmt.Errorf("failed to get MaaSModelRef %s/%s: %w", modelNamespace, modelName, err)
	}
	if model.Spec.ModelRef.Kind != "LLMInferenceService" {
		return nil, maasv1alpha1.FlowControlStateNotApplicable,
			fmt.Sprintf("model kind %s is not served through an inference scheduler", model.Spec.ModelRef.Kind), nil
	}

	llmisvc := &kservev1alpha2.LLMInferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: model.Namespace, Name: model.Spec.ModelRef.Name}, llmisvc); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil, maasv1alpha1.FlowControlStatePending,
				fmt.Sprintf("LLMInferenceService %s/%s not found", model.Namespace, model.Spec.ModelRef.Name), nil
		}
		return nil, maasv1alpha1.FlowControlStatePending, fmt.Sprintf("failed to get LLMInferenceService: %v", err),
			fmt.Errorf("failed to get LLMInferenceService %s/%s: %w", model.Namespace, model.Spec.ModelRef.Name, err)
	}

	router := llmisvc.Status.Router
	if router != nil && router.Group != nil && activeRoutingGroupMembers(router.Group) > 1 {
		return nil, maasv1alpha1.FlowControlStateUnsupported,
			fmt.Sprintf("LLMInferenceService %s/%s splits traffic across routing group %s; flow control requires a single InferencePool",
				llmisvc.Namespace, llmisvc.Name, router.Group.Name), nil
	}
	if router != nil && router.Scheduler != nil && router.Scheduler.InferencePool != nil {
		observed := router.Scheduler.InferencePool
		pool := &maasv1alpha1.InferencePoolReference{
			Group:     string(observed.Group),
			Kind:      string(observed.Kind),
			Name:      string(observed.Name),
			Namespace: llmisvc.Namespace,
		}
		if observed.Namespace != nil && *observed.Namespace != "" {
			pool.Namespace = string(*observed.Namespace)
		}
		return pool, "", "", nil
	}

	// The scheduler can come from the service spec or from a referenced config, so rely on
	// the observed status: a ready service without a pool is served without a scheduler.
	if llmisvcReadyStatus(llmisvc) != string(corev1.ConditionTrue) {
		return nil, maasv1alpha1.FlowControlStatePending,
			fmt.Sprintf("waiting for LLMInferenceService %s/%s to report an InferencePool", llmisvc.Namespace, llmisvc.Name), nil
	}
	return nil, maasv1alpha1.FlowControlStateNotApplicable,
		fmt.Sprintf("LLMInferenceService %s/%s is not served through an inference scheduler", llmisvc.Namespace, llmisvc.Name), nil
}

// activeRoutingGroupMembers counts routing group members that receive traffic.
func activeRoutingGroupMembers(group *kservev1alpha2.GroupStatus) int {
	active := 0
	for _, m := range group.Members {
		if !m.Stopped && m.Weight > 0 {
			active++
		}
	}
	return active
}

// mapLLMISvcToMaaSSubscriptions returns reconcile requests for the MaaSSubscriptions that
// reference MaaSModelRefs backed by the LLMInferenceService.
func (r *MaaSSubscriptionReconciler) mapLLMISvcToMaaSSubscriptions(ctx context.Context, obj client.Object) []reconcile.Request {
	var models maasv1alpha1.MaaSModelRefList
	if err := r.List(ctx, &models, client.InNamespace(obj.GetNamespace())); err != nil {
		oteljson.FromContext(ctx).Error(err, "failed to list MaaSModelRefs for LLMInferenceService", "llmisvc", qualifiedName(obj.GetNamespace(), obj.GetName()))
		return nil
	}
	seen := make(map[types.NamespacedName]struct{})
	var requests []reconcile.Request
	for i := range models.Items {
		m := &models.Items[i]
		if m.Spec.ModelRef.Kind != "LLMInferenceService" || m.Spec.ModelRef.Name != obj.GetName() {
			continue
		}
		for _, req := range r.mapMaaSModelRefToMaaSSubscriptions(ctx, m) {
			if _, ok := seen[req.NamespacedName]; ok {
				continue
			}
			seen[req.NamespacedName] = struct{}{}
			requests = append(requests, req)
		}
	}
	return requests
}

// llmisvcRouterStatusChangedPredicate passes Create/Delete events and Update events where the
// LLMInferenceService's observed router topology or Ready condition changed.
type llmisvcRouterStatusChangedPredicate struct {
	predicate.Funcs
}

func (llmisvcRouterStatusChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}
	if oldObj, ok := e.ObjectOld.(*kservev1alpha2.LLMInferenceService); ok {
		newObj, ok := e.ObjectNew.(*kservev1alpha2.LLMInferenceService)
		if !ok {
			return true
		}
		return !equality.Semantic.DeepEqual(oldObj.Status.Router, newObj.Status.Router) ||
			llmisvcReadyStatus(oldObj) != llmisvcReadyStatus(newObj)
	}
	// Dynamic watches registered after the CRD appears deliver unstructured objects.
	oldU, ok := e.ObjectOld.(*unstructured.Unstructured)
	if !ok {
		return true
	}
	newU, ok := e.ObjectNew.(*unstructured.Unstructured)
	if !ok {
		return true
	}
	oldRouter, _, _ := unstructured.NestedFieldNoCopy(oldU.Object, "status", "router")
	newRouter, _, _ := unstructured.NestedFieldNoCopy(newU.Object, "status", "router")
	return !equality.Semantic.DeepEqual(oldRouter, newRouter) ||
		unstructuredLLMIsvcReadyStatus(oldU) != unstructuredLLMIsvcReadyStatus(newU)
}
