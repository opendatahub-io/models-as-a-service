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
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// MaaSSubscriptionReconciler reconciles a MaaSSubscription object
type MaaSSubscriptionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=tokenratelimitpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update

const (
	maasSubscriptionFinalizer = "maas.opendatahub.io/subscription-cleanup"
	// modelRefIndexKey is the field index key for looking up MaaSSubscriptions by model reference.
	// The index value format is "namespace/name" of the model.
	modelRefIndexKey = "spec.modelRef"
)

// ConditionSpecPriorityDuplicate is set True when another MaaSSubscription shares the same spec.priority
// (API key mint and selector use deterministic tie-break; admins should set distinct priorities).
const ConditionSpecPriorityDuplicate = "SpecPriorityDuplicate"

// validateModelRefs checks each model reference and returns per-model status.
func (r *MaaSSubscriptionReconciler) validateModelRefs(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription) []maasv1alpha1.ModelRefStatus {
	statuses := make([]maasv1alpha1.ModelRefStatus, 0, len(subscription.Spec.ModelRefs))
	seen := make(map[string]struct{})

	for _, ref := range subscription.Spec.ModelRefs {
		key := ref.Namespace + "/" + ref.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		status := maasv1alpha1.ModelRefStatus{
			ResourceRefStatus: maasv1alpha1.ResourceRefStatus{
				Name:      ref.Name,
				Namespace: ref.Namespace,
			},
		}

		model := &maasv1alpha1.MaaSModelRef{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, model); err != nil {
			if apierrors.IsNotFound(err) {
				status.Ready = false
				status.Reason = maasv1alpha1.ReasonNotFound
				status.Message = fmt.Sprintf("MaaSModelRef %s/%s not found", ref.Namespace, ref.Name)
			} else {
				status.Ready = false
				status.Reason = maasv1alpha1.ReasonGetFailed
				status.Message = fmt.Sprintf("failed to get MaaSModelRef: %v", err)
			}
		} else {
			status.Ready = true
			status.Reason = maasv1alpha1.ReasonValid
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// checkTokenRateLimitHealth checks the health of generated TokenRateLimitPolicies.
func (r *MaaSSubscriptionReconciler) checkTokenRateLimitHealth(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription) []maasv1alpha1.TokenRateLimitStatus {
	statuses := make([]maasv1alpha1.TokenRateLimitStatus, 0, len(subscription.Spec.ModelRefs))
	seen := make(map[string]struct{})

	for _, ref := range subscription.Spec.ModelRefs {
		key := ref.Namespace + "/" + ref.Name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		policyName := fmt.Sprintf("maas-trlp-%s", ref.Name)
		status := maasv1alpha1.TokenRateLimitStatus{
			ResourceRefStatus: maasv1alpha1.ResourceRefStatus{
				Name: policyName,
			},
			Model: ref.Name,
		}

		// Find the TRLP for this model (TRLP lives in HTTPRoute namespace)
		_, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, ref.Namespace, ref.Name)
		if err != nil {
			// Record status even when HTTPRoute not found - makes diagnosing issues easier
			status.Ready = false
			if errors.Is(err, ErrHTTPRouteNotFound) || errors.Is(err, ErrModelNotFound) {
				status.Reason = maasv1alpha1.ReasonBackendNotReady
				status.Message = fmt.Sprintf("HTTPRoute not found yet; TokenRateLimitPolicy cannot be created: %v", err)
			} else {
				status.Reason = maasv1alpha1.ReasonGetFailed
				status.Message = fmt.Sprintf("failed to find HTTPRoute for model: %v", err)
			}
			statuses = append(statuses, status)
			continue
		}
		status.Namespace = httpRouteNS

		trlp := &unstructured.Unstructured{}
		trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})

		if err := r.Get(ctx, types.NamespacedName{Name: policyName, Namespace: httpRouteNS}, trlp); err != nil {
			if apierrors.IsNotFound(err) {
				status.Ready = false
				status.Reason = maasv1alpha1.ReasonNotFound
				status.Message = "TokenRateLimitPolicy not created yet"
			} else {
				status.Ready = false
				status.Reason = maasv1alpha1.ReasonGetFailed
				status.Message = fmt.Sprintf("failed to get TokenRateLimitPolicy: %v", err)
			}
		} else {
			// Check Accepted condition from TRLP status
			accepted, message := getTRLPAcceptedCondition(trlp)
			status.Ready = accepted
			if accepted {
				status.Reason = maasv1alpha1.ReasonAccepted
			} else {
				status.Reason = maasv1alpha1.ReasonNotAccepted
				status.Message = message
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// getTRLPAcceptedCondition extracts the Accepted condition from a TokenRateLimitPolicy.
func getTRLPAcceptedCondition(trlp *unstructured.Unstructured) (accepted bool, message string) {
	status, found, err := unstructured.NestedMap(trlp.Object, "status")
	if err != nil || !found {
		return false, "status not available"
	}

	conditions, found, err := unstructured.NestedSlice(status, "conditions")
	if err != nil || !found {
		return false, "conditions not found"
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Accepted" {
			if cond["status"] == "True" {
				return true, ""
			}
			if msg, ok := cond["message"].(string); ok {
				return false, msg
			}
			return false, "Accepted condition is False"
		}
	}
	return false, "Accepted condition not found"
}

// deriveFinalPhase determines the subscription phase based on model and TRLP statuses.
func deriveFinalPhase(modelStatuses []maasv1alpha1.ModelRefStatus, trlpStatuses []maasv1alpha1.TokenRateLimitStatus) (phase maasv1alpha1.Phase, message string) {
	if len(modelStatuses) == 0 {
		return maasv1alpha1.PhaseFailed, "no model references specified"
	}

	// Build a set of models that validateModelRefs reported as valid
	validModelSet := make(map[string]struct{})
	var validModels, invalidModels int
	for _, s := range modelStatuses {
		if s.Ready {
			validModels++
			validModelSet[s.Name] = struct{}{}
		} else {
			invalidModels++
		}
	}

	// Check TRLP health
	// Also detect race condition: model reported as valid by validateModelRefs but
	// deleted before checkTokenRateLimitHealth ran (TRLP reports BackendNotReady)
	var healthyTRLPs, unhealthyTRLPs, modelsWithBackendIssues int
	for _, s := range trlpStatuses {
		if s.Ready {
			healthyTRLPs++
		} else {
			unhealthyTRLPs++
			// Only count as backend issue if the model was reported as valid
			// (avoids double-counting models already marked as invalid)
			if s.Reason == maasv1alpha1.ReasonBackendNotReady {
				if _, wasValid := validModelSet[s.Model]; wasValid {
					modelsWithBackendIssues++
				}
			}
		}
	}

	// Adjust counts for race condition: models thought to be valid but actually unavailable
	effectiveValidModels := validModels - modelsWithBackendIssues
	effectiveInvalidModels := invalidModels + modelsWithBackendIssues

	// All models invalid -> Failed
	if effectiveValidModels <= 0 {
		return maasv1alpha1.PhaseFailed, fmt.Sprintf("all %d model references are invalid or unavailable", len(modelStatuses))
	}

	// Partial model failure -> Degraded
	if effectiveInvalidModels > 0 {
		return maasv1alpha1.PhaseDegraded, fmt.Sprintf("%d of %d model references are invalid or unavailable", effectiveInvalidModels, len(modelStatuses))
	}

	// All models valid but some TRLPs unhealthy (not due to backend issues) -> Degraded
	trlpOnlyIssues := unhealthyTRLPs - modelsWithBackendIssues
	if trlpOnlyIssues > 0 {
		return maasv1alpha1.PhaseDegraded, fmt.Sprintf("%d of %d TokenRateLimitPolicies not accepted", trlpOnlyIssues, len(trlpStatuses))
	}

	return maasv1alpha1.PhaseActive, "successfully reconciled"
}

// Reconcile is part of the main kubernetes reconciliation loop
func (r *MaaSSubscriptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSSubscription", req.NamespacedName)

	subscription := &maasv1alpha1.MaaSSubscription{}
	if err := r.Get(ctx, req.NamespacedName, subscription); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSSubscription")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !subscription.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, subscription)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(subscription, maasSubscriptionFinalizer) {
		controllerutil.AddFinalizer(subscription, maasSubscriptionFinalizer)
		if err := r.Update(ctx, subscription); err != nil {
			return ctrl.Result{}, err
		}
	}

	statusSnapshot := subscription.Status.DeepCopy()

	// Validate model references and populate per-model status
	modelStatuses := r.validateModelRefs(ctx, subscription)
	subscription.Status.ModelRefStatuses = modelStatuses

	// Check if we have any valid models to proceed with TRLP reconciliation
	hasValidModels := false
	for _, s := range modelStatuses {
		if s.Ready {
			hasValidModels = true
			break
		}
	}

	// Only reconcile TRLPs if we have valid models
	if hasValidModels {
		// Reconcile TokenRateLimitPolicy for each model
		// IMPORTANT: TokenRateLimitPolicy targets the HTTPRoute for each model
		if err := r.reconcileTokenRateLimitPolicies(ctx, log, subscription); err != nil {
			log.Error(err, "failed to reconcile TokenRateLimitPolicies")
			subscription.Status.Phase = maasv1alpha1.PhaseFailed
			r.updateStatus(ctx, subscription, maasv1alpha1.PhaseFailed, fmt.Sprintf("failed to reconcile TokenRateLimitPolicies: %v", err), statusSnapshot)
			return ctrl.Result{}, err
		}
	} else {
		// No valid models - clean up any stale TRLPs from previous reconciliations
		if err := r.cleanupStaleTRLPs(ctx, log, subscription); err != nil {
			log.Error(err, "failed to clean up stale TokenRateLimitPolicies")
			r.updateStatus(ctx, subscription, maasv1alpha1.PhaseFailed, fmt.Sprintf("failed to clean up stale TokenRateLimitPolicies: %v", err), statusSnapshot)
			return ctrl.Result{}, err
		}
	}

	// Check TRLP health and populate status
	trlpStatuses := r.checkTokenRateLimitHealth(ctx, subscription)
	subscription.Status.TokenRateLimitStatuses = trlpStatuses

	// Derive final phase based on model and TRLP health
	phase, message := deriveFinalPhase(modelStatuses, trlpStatuses)
	r.updateStatus(ctx, subscription, phase, message, statusSnapshot)

	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) reconcileTokenRateLimitPolicies(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) error {
	// Model-centric approach: for each model referenced by this subscription,
	// find ALL subscriptions for that model and build a single aggregated TokenRateLimitPolicy.
	// Kuadrant only allows one TokenRateLimitPolicy per HTTPRoute target.

	// Deduplicate model references to prevent reconciling the same model multiple times
	seen := make(map[string]struct{}, len(subscription.Spec.ModelRefs))
	for _, modelRef := range subscription.Spec.ModelRefs {
		k := modelRef.Namespace + "/" + modelRef.Name
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		if err := r.reconcileTRLPForModel(ctx, log, modelRef.Namespace, modelRef.Name); err != nil {
			return err
		}
	}
	if err := r.cleanupStaleTRLPs(ctx, log, subscription); err != nil {
		return err
	}
	return nil
}

// reconcileTRLPForModel builds or updates the aggregated TokenRateLimitPolicy for a specific model.
// It finds all active subscriptions for the model and creates a single TRLP covering all of them.
func (r *MaaSSubscriptionReconciler) reconcileTRLPForModel(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	// Find ALL subscriptions for this model (not just the current one)
	allSubs, err := findAllSubscriptionsForModel(ctx, r.Client, modelNamespace, modelName)
	if err != nil {
		return fmt.Errorf("failed to list subscriptions for model %s/%s: %w", modelNamespace, modelName, err)
	}

	// Resolve HTTPRoute early to check if model/route exist
	httpRouteName, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, modelNamespace, modelName)
	if err != nil {
		// During cleanup (model not found or no subscriptions), treat missing HTTPRoute as non-fatal.
		// The TRLP can still be deleted using model labels without needing the HTTPRoute.
		if errors.Is(err, ErrModelNotFound) || len(allSubs) == 0 {
			log.Info("model/route not found during cleanup, deleting TokenRateLimitPolicy via labels", "model", modelNamespace+"/"+modelName, "error", err.Error())
			if delErr := r.deleteModelTRLP(ctx, log, modelNamespace, modelName); delErr != nil {
				return fmt.Errorf("failed to clean up TokenRateLimitPolicy for missing model %s/%s: %w", modelNamespace, modelName, delErr)
			}
			return nil
		}
		if errors.Is(err, ErrHTTPRouteNotFound) {
			// HTTPRoute doesn't exist yet - skip for now. HTTPRoute watch will trigger reconciliation when route is created.
			log.Info("HTTPRoute not found for model, skipping TokenRateLimitPolicy creation", "model", modelNamespace+"/"+modelName)
			return nil
		}
		return fmt.Errorf("failed to resolve HTTPRoute for model %s/%s: %w", modelNamespace, modelName, err)
	}

	// Check if existing TRLP is opted-out before doing any expensive work
	policyName := fmt.Sprintf("maas-trlp-%s", modelName)
	existingCheck := &unstructured.Unstructured{}
	existingCheck.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	existingCheck.SetName(policyName)
	existingCheck.SetNamespace(httpRouteNS)
	if err := r.Get(ctx, client.ObjectKeyFromObject(existingCheck), existingCheck); err == nil {
		if !isManaged(existingCheck) {
			log.Info("TokenRateLimitPolicy opted out, skipping reconciliation", "name", policyName, "namespace", httpRouteNS, "model", modelNamespace+"/"+modelName)
			return nil
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing TokenRateLimitPolicy: %w", err)
	}

	// If no subscriptions remain, delete the TRLP
	if len(allSubs) == 0 {
		log.Info("no active subscriptions for model, deleting TokenRateLimitPolicy", "model", modelNamespace+"/"+modelName)
		if delErr := r.deleteModelTRLP(ctx, log, modelNamespace, modelName); delErr != nil {
			return fmt.Errorf("failed to delete TokenRateLimitPolicy for model %s/%s: %w", modelNamespace, modelName, delErr)
		}
		return nil
	}

	// Fetch the HTTPRoute to set as owner for garbage collection
	route := &gatewayapiv1.HTTPRoute{}
	if err := r.Get(ctx, types.NamespacedName{Name: httpRouteName, Namespace: httpRouteNS}, route); err != nil {
		return fmt.Errorf("failed to fetch HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}

	limitsMap := map[string]any{}
	var subNames []string

	type subInfo struct {
		sub   maasv1alpha1.MaaSSubscription
		mRef  maasv1alpha1.ModelSubscriptionRef
		rates []any
	}
	var subs []subInfo
	for _, sub := range allSubs {
		for _, mRef := range sub.Spec.ModelRefs {
			if mRef.Namespace != modelNamespace || mRef.Name != modelName {
				continue
			}
			var rates []any
			if len(mRef.TokenRateLimits) > 0 {
				for _, trl := range mRef.TokenRateLimits {
					rates = append(rates, map[string]any{"limit": trl.Limit, "window": trl.Window})
				}
			} else {
				rates = append(rates, map[string]any{"limit": int64(100), "window": "1m"})
			}
			subs = append(subs, subInfo{sub: sub, mRef: mRef, rates: rates})
			break
		}
	}

	// Trust auth.identity.selected_subscription_key from AuthPolicy.
	// AuthPolicy has already validated subscription selection via /v1/subscriptions/select,
	// which handles:
	//  - Validating subscription exists and user has access (groups/users match)
	//  - Auto-selecting if user has exactly one subscription
	//  - Returning 403 Forbidden for invalid scenarios (wrong header, no access, multiple without header)
	// TokenRateLimitPolicy simply applies the rate limit for the validated subscription.
	//
	// The selected_subscription_key format is: {subNamespace}/{subName}@{modelNamespace}/{modelName}
	// This ensures proper isolation between subscriptions in different namespaces and across models.
	for _, si := range subs {
		subNames = append(subNames, si.sub.Name)

		// Build subscription reference: namespace/name
		subRef := fmt.Sprintf("%s/%s", si.sub.Namespace, si.sub.Name)
		// Build model-scoped reference: subscription@model
		modelScopedRef := fmt.Sprintf("%s@%s/%s", subRef, si.mRef.Namespace, si.mRef.Name)

		// TRLP limit key must be safe for YAML (no slashes)
		safeKey := strings.ReplaceAll(subRef, "/", "-")
		limitsMap[fmt.Sprintf("%s-%s-tokens", safeKey, si.mRef.Name)] = map[string]any{
			"rates": si.rates,
			"when": []any{
				map[string]any{
					"predicate": fmt.Sprintf(`auth.identity.selected_subscription_key == "%s"`, modelScopedRef),
				},
			},
			"counters": []any{
				map[string]any{"expression": "auth.identity.userid"},
			},
		}
	}

	// Sort subscription names for stable annotation value across reconciles
	sort.Strings(subNames)

	// Build the aggregated TokenRateLimitPolicy (one per model, covering all subscriptions)
	// policyName already declared during early opt-out check
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	policy.SetName(policyName)
	policy.SetNamespace(httpRouteNS)
	policy.SetLabels(map[string]string{
		"maas.opendatahub.io/model":           modelName,
		"maas.opendatahub.io/model-namespace": modelNamespace,
		"app.kubernetes.io/managed-by":        "maas-controller",
		"app.kubernetes.io/part-of":           "maas-subscription",
		"app.kubernetes.io/component":         "token-rate-limit-policy",
	})
	policy.SetAnnotations(map[string]string{
		"maas.opendatahub.io/subscriptions": strings.Join(subNames, ","),
	})

	// Set HTTPRoute as owner for garbage collection (TRLP deleted when route is deleted)
	if err := controllerutil.SetControllerReference(route, policy, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on TokenRateLimitPolicy %s/%s: %w", policy.GetNamespace(), policy.GetName(), err)
	}

	spec := map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "HTTPRoute",
			"name":  httpRouteName,
		},
		"limits": limitsMap,
	}
	if err := unstructured.SetNestedMap(policy.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set spec: %w", err)
	}

	// Create or update TokenRateLimitPolicy
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(policy.GroupVersionKind())
	err = r.Get(ctx, client.ObjectKeyFromObject(policy), existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, policy); err != nil {
			return fmt.Errorf("failed to create TokenRateLimitPolicy for model %s: %w", modelName, err)
		}
		log.Info("TokenRateLimitPolicy created", "name", policyName, "model", modelName, "subscriptionCount", len(subNames), "subscriptions", subNames)
	} else if err != nil {
		return fmt.Errorf("failed to get existing TokenRateLimitPolicy: %w", err)
	} else {
		// Double-check managed status as a safety check for races (TRLP could have been
		// opted-out between the early check and now).
		if !isManaged(existing) {
			log.Info("TokenRateLimitPolicy opted out during reconciliation, skipping update", "name", policyName)
		} else {
			// Ensure owner reference is set on managed existing policy.
			if err := controllerutil.SetControllerReference(route, existing, r.Scheme); err != nil {
				return fmt.Errorf("failed to set owner reference on existing TokenRateLimitPolicy %s/%s: %w", existing.GetNamespace(), existing.GetName(), err)
			}
			// Snapshot the existing object before modifications so we can detect
			// no-op updates.
			snapshot := existing.DeepCopy()

			mergedAnnotations := existing.GetAnnotations()
			if mergedAnnotations == nil {
				mergedAnnotations = make(map[string]string)
			}
			for k, v := range policy.GetAnnotations() {
				mergedAnnotations[k] = v
			}
			existing.SetAnnotations(mergedAnnotations)

			mergedLabels := existing.GetLabels()
			if mergedLabels == nil {
				mergedLabels = make(map[string]string)
			}
			for k, v := range policy.GetLabels() {
				mergedLabels[k] = v
			}
			existing.SetLabels(mergedLabels)
			if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
				return fmt.Errorf("failed to update spec: %w", err)
			}

			if equality.Semantic.DeepEqual(snapshot.Object, existing.Object) {
				log.Info("TokenRateLimitPolicy unchanged, skipping update", "name", policyName, "model", modelNamespace+"/"+modelName, "subscriptionCount", len(subNames))
			} else {
				if err := r.Update(ctx, existing); err != nil {
					return fmt.Errorf("failed to update TokenRateLimitPolicy for model %s/%s: %w", modelNamespace, modelName, err)
				}
				log.Info("TokenRateLimitPolicy updated", "name", policyName, "model", modelNamespace+"/"+modelName, "subscriptionCount", len(subNames), "subscriptions", subNames)
			}
		}
	}
	return nil
}

// cleanupStaleTRLPs deletes aggregated TokenRateLimitPolicies for models that this
// subscription previously contributed to but no longer references in spec.modelRefs.
// Generated TRLPs track contributing subscriptions in the
// "maas.opendatahub.io/subscriptions" annotation.
func (r *MaaSSubscriptionReconciler) cleanupStaleTRLPs(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) error {
	currentModels := make(map[string]bool, len(subscription.Spec.ModelRefs))
	for _, ref := range subscription.Spec.ModelRefs {
		currentModels[ref.Namespace+"/"+ref.Name] = true
	}

	allManaged := &unstructured.UnstructuredList{}
	allManaged.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"})
	if err := r.List(ctx, allManaged, client.MatchingLabels{
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-subscription",
	}); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list managed TokenRateLimitPolicies for stale cleanup: %w", err)
	}

	for i := range allManaged.Items {
		trlp := &allManaged.Items[i]
		modelName := trlp.GetLabels()["maas.opendatahub.io/model"]
		if modelName == "" {
			continue
		}
		modelKey := trlp.GetNamespace() + "/" + modelName
		if currentModels[modelKey] {
			continue
		}
		if !slices.Contains(strings.Split(trlp.GetAnnotations()["maas.opendatahub.io/subscriptions"], ","), subscription.Name) {
			continue
		}
		log.Info("Cleaning up stale TokenRateLimitPolicy for removed modelRef", "model", modelKey, "trlp", trlp.GetName())
		if err := r.deleteModelTRLP(ctx, log, trlp.GetNamespace(), modelName); err != nil {
			return fmt.Errorf("failed to clean up stale TokenRateLimitPolicy for removed model %s: %w", modelKey, err)
		}
	}
	return nil
}

// deleteModelTRLP deletes the aggregated TokenRateLimitPolicy for a model in the given namespace.
func (r *MaaSSubscriptionReconciler) deleteModelTRLP(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	// Always delete the aggregated TokenRateLimitPolicy so remaining MaaSSubscriptions rebuild it
	// without the rate limits from the deleted subscription. If we skip deletion, the aggregated
	// TokenRateLimitPolicy will contain stale configuration from the deleted MaaSSubscription.
	//
	// Search across all namespaces using model labels since TRLP is created in HTTPRoute namespace
	// (not model namespace). This allows cleanup even when HTTPRoute is already deleted.
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":           modelName,
		"maas.opendatahub.io/model-namespace": modelNamespace,
		"app.kubernetes.io/managed-by":        "maas-controller",
		"app.kubernetes.io/part-of":           "maas-subscription",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list TokenRateLimitPolicy for cleanup: %w", err)
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		if !isManaged(p) {
			log.Info("TokenRateLimitPolicy opted out, skipping deletion", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelNamespace+"/"+modelName)
			continue
		}
		log.Info("Deleting TokenRateLimitPolicy (no remaining parent subscriptions)", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelNamespace+"/"+modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete TokenRateLimitPolicy %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}

func (r *MaaSSubscriptionReconciler) handleDeletion(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(subscription, maasSubscriptionFinalizer) {
		// For each model referenced by this subscription, rebuild the aggregated TokenRateLimitPolicy
		// without the deleted subscription's limits. If no other subscriptions reference the model,
		// the TRLP will be deleted. This ensures zero-downtime rate limiting during subscription removal.
		seen := make(map[string]struct{}, len(subscription.Spec.ModelRefs))
		for _, modelRef := range subscription.Spec.ModelRefs {
			k := modelRef.Namespace + "/" + modelRef.Name
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			log.Info("Rebuilding TokenRateLimitPolicy without deleted subscription", "model", modelRef.Namespace+"/"+modelRef.Name, "subscription", subscription.Name)
			if err := r.reconcileTRLPForModel(ctx, log, modelRef.Namespace, modelRef.Name); err != nil {
				log.Error(err, "failed to reconcile TokenRateLimitPolicy during deletion, will retry", "model", modelRef.Namespace+"/"+modelRef.Name)
				return ctrl.Result{}, err
			}
		}
		// Also clean up stale TRLPs from modelRefs that were removed
		// before the CR was deleted (edge case: edit + delete before reconcile).
		if err := r.cleanupStaleTRLPs(ctx, log, subscription); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(subscription, maasSubscriptionFinalizer)
		if err := r.Update(ctx, subscription); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) updateStatus(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription, phase maasv1alpha1.Phase, message string, statusSnapshot *maasv1alpha1.MaaSSubscriptionStatus) {
	// Status-only updates do not bump metadata.generation, so this reconcile may not re-queue.
	// Merge SpecPriorityDuplicate from the API server so we do not clobber the async duplicate-priority scan.
	latest := &maasv1alpha1.MaaSSubscription{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(subscription), latest); err == nil {
		if dup := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionSpecPriorityDuplicate); dup != nil {
			apimeta.SetStatusCondition(&subscription.Status.Conditions, *dup)
		}
	}

	subscription.Status.Phase = phase

	var status metav1.ConditionStatus
	var reason maasv1alpha1.ConditionReason
	switch phase {
	case maasv1alpha1.PhaseActive:
		status = metav1.ConditionTrue
		reason = maasv1alpha1.ReasonReconciled
	case maasv1alpha1.PhaseDegraded:
		status = metav1.ConditionFalse
		reason = maasv1alpha1.ReasonPartialFailure
	case maasv1alpha1.PhaseFailed:
		status = metav1.ConditionFalse
		reason = maasv1alpha1.ReasonReconcileFailed
	default:
		status = metav1.ConditionUnknown
		reason = maasv1alpha1.ReasonUnknown
	}

	apimeta.SetStatusCondition(&subscription.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: subscription.GetGeneration(),
	})

	if equality.Semantic.DeepEqual(*statusSnapshot, subscription.Status) {
		return
	}

	if err := r.Status().Update(ctx, subscription); err != nil {
		log := logr.FromContextOrDiscard(ctx)
		log.Error(err, "failed to update MaaSSubscription status", "name", subscription.Name)
	}
}

// scanForDuplicatePriority lists live MaaSSubscriptions and sets SpecPriorityDuplicate
// on each. Triggered on create, delete, or when spec.priority changes (see SetupWithManager).
func (r *MaaSSubscriptionReconciler) scanForDuplicatePriority(ctx context.Context) {
	log := logr.FromContextOrDiscard(ctx).WithName("MaaSSubscriptionDuplicatePriority")
	var list maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &list); err != nil {
		log.Error(err, "failed to list MaaSSubscriptions for duplicate priority scan")
		return
	}

	liveIdx := make([]int, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			liveIdx = append(liveIdx, i)
		}
	}

	byPriority := make(map[int32][]string)
	for _, i := range liveIdx {
		s := &list.Items[i]
		p := s.Spec.Priority
		k := s.Namespace + "/" + s.Name
		byPriority[p] = append(byPriority[p], k)
	}
	for p := range byPriority {
		sort.Strings(byPriority[p])
	}

	var duplicateDetails []string
	for p, keys := range byPriority {
		if len(keys) > 1 {
			duplicateDetails = append(duplicateDetails, fmt.Sprintf("priority=%d:%v", p, keys))
		}
	}
	sort.Strings(duplicateDetails)
	if len(duplicateDetails) > 0 {
		log.Info("duplicate MaaSSubscription spec.priority groups — resolve ties for predictable API key mint / subscription selection",
			"groups", duplicateDetails)
	}

	for _, i := range liveIdx {
		s := &list.Items[i]
		selfKey := s.Namespace + "/" + s.Name
		p := s.Spec.Priority
		keys := byPriority[p]
		var peers []string
		for _, k := range keys {
			if k != selfKey {
				peers = append(peers, k)
			}
		}

		latest := &maasv1alpha1.MaaSSubscription{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, latest); err != nil {
			log.Error(err, "failed to get MaaSSubscription for duplicate priority status patch", "subscription", selfKey)
			continue
		}
		if !latest.DeletionTimestamp.IsZero() {
			continue
		}

		gen := latest.GetGeneration()
		var desired metav1.Condition
		if len(peers) == 0 {
			desired = metav1.Condition{
				Type:               ConditionSpecPriorityDuplicate,
				Status:             metav1.ConditionFalse,
				Reason:             "NoDuplicatePeers",
				Message:            "",
				ObservedGeneration: gen,
			}
		} else {
			desired = metav1.Condition{
				Type:               ConditionSpecPriorityDuplicate,
				Status:             metav1.ConditionTrue,
				Reason:             "SharedPriority",
				Message:            fmt.Sprintf("spec.priority %d is shared with: %s", p, strings.Join(peers, ", ")),
				ObservedGeneration: gen,
			}
		}

		cur := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionSpecPriorityDuplicate)
		if conditionsSemanticallyEqual(cur, &desired) {
			continue
		}
		apimeta.SetStatusCondition(&latest.Status.Conditions, desired)
		if err := r.Status().Update(ctx, latest); err != nil {
			log.Error(err, "failed to update SpecPriorityDuplicate status", "subscription", selfKey)
		}
	}
}

func conditionsSemanticallyEqual(a, b *metav1.Condition) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Type == b.Type && a.Status == b.Status && a.Reason == b.Reason && a.Message == b.Message && a.ObservedGeneration == b.ObservedGeneration
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSSubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register field indexer for efficient lookup of MaaSSubscriptions by model reference.
	// This avoids cluster-wide scans when finding subscriptions for a specific model.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&maasv1alpha1.MaaSSubscription{},
		modelRefIndexKey,
		func(obj client.Object) []string {
			sub, ok := obj.(*maasv1alpha1.MaaSSubscription)
			if !ok {
				return nil
			}
			var refs []string
			for _, modelRef := range sub.Spec.ModelRefs {
				// Index value format: "namespace/name"
				refs = append(refs, modelRef.Namespace+"/"+modelRef.Name)
			}
			return refs
		},
	); err != nil {
		return fmt.Errorf("failed to setup field indexer for MaaSSubscription: %w", err)
	}

	// Watch generated TokenRateLimitPolicies so we re-reconcile when someone manually edits them.
	generatedTRLP := &unstructured.Unstructured{}
	generatedTRLP.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSSubscription{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.Funcs{UpdateFunc: deletionTimestampSet},
		))).
		// Full scan of duplicate spec.priority on create, delete, or priority-only spec update.
		// Does not enqueue reconciles; only patches status conditions on all subscriptions.
		Watches(
			&maasv1alpha1.MaaSSubscription{},
			duplicatePriorityScanHandler(r),
			builder.WithPredicates(duplicatePriorityScanPredicate()),
		).
		// Watch HTTPRoutes so we re-reconcile when KServe creates/updates a route
		// (fixes race condition where MaaSSubscription is created before HTTPRoute exists).
		Watches(&gatewayapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(
			r.mapHTTPRouteToMaaSSubscriptions,
		)).
		// Watch MaaSModelRefs so we re-reconcile when a model is created or deleted.
		Watches(&maasv1alpha1.MaaSModelRef{}, handler.EnqueueRequestsFromMapFunc(
			r.mapMaaSModelRefToMaaSSubscriptions,
		)).
		// Watch generated TokenRateLimitPolicies so manual edits get overwritten by the controller.
		Watches(generatedTRLP, handler.EnqueueRequestsFromMapFunc(
			r.mapGeneratedTRLPToParent,
		)).
		Complete(r)
}

// duplicatePriorityScanHandler runs a full duplicate-priority scan without enqueuing reconciles.
func duplicatePriorityScanHandler(r *MaaSSubscriptionReconciler) handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, _ event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.scanForDuplicatePriority(ctx)
		},
		UpdateFunc: func(ctx context.Context, _ event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.scanForDuplicatePriority(ctx)
		},
		DeleteFunc: func(ctx context.Context, _ event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.scanForDuplicatePriority(ctx)
		},
	}
}

// duplicatePriorityScanPredicate limits full scans to subscription lifecycle / priority changes.
func duplicatePriorityScanPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSub, ok1 := e.ObjectOld.(*maasv1alpha1.MaaSSubscription)
			newSub, ok2 := e.ObjectNew.(*maasv1alpha1.MaaSSubscription)
			if !ok1 || !ok2 {
				return false
			}
			return oldSub.Spec.Priority != newSub.Spec.Priority
		},
		DeleteFunc: func(event.DeleteEvent) bool { return true },
	}
}

// mapGeneratedTRLPToParent maps a generated TokenRateLimitPolicy back to any
// MaaSSubscription that references the same model. The TokenRateLimitPolicy is per-model (aggregated),
// so we use the model label to find a subscription to trigger reconciliation.
func (r *MaaSSubscriptionReconciler) mapGeneratedTRLPToParent(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "maas-controller" {
		return nil
	}
	modelName := labels["maas.opendatahub.io/model"]
	if modelName == "" {
		return nil
	}
	modelNamespace := obj.GetNamespace()
	sub := findAnySubscriptionForModel(ctx, r.Client, modelNamespace, modelName)
	if sub == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: sub.Name, Namespace: sub.Namespace},
	}}
}

// mapMaaSModelRefToMaaSSubscriptions returns reconcile requests for all MaaSSubscriptions
// that reference the given MaaSModelRef.
func (r *MaaSSubscriptionReconciler) mapMaaSModelRefToMaaSSubscriptions(ctx context.Context, obj client.Object) []reconcile.Request {
	model, ok := obj.(*maasv1alpha1.MaaSModelRef)
	if !ok {
		return nil
	}
	// Use field indexer to efficiently find subscriptions for this specific model
	modelKey := model.Namespace + "/" + model.Name
	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions, client.MatchingFields{modelRefIndexKey: modelKey}); err != nil {
		return nil
	}
	// Deduplicate requests (same subscription shouldn't be queued multiple times)
	seen := make(map[types.NamespacedName]struct{}, len(subscriptions.Items))
	var requests []reconcile.Request
	for _, s := range subscriptions.Items {
		key := types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

// mapHTTPRouteToMaaSSubscriptions returns reconcile requests for all MaaSSubscriptions
// that reference models in the HTTPRoute's namespace.
func (r *MaaSSubscriptionReconciler) mapHTTPRouteToMaaSSubscriptions(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayapiv1.HTTPRoute)
	if !ok {
		return nil
	}
	// Find MaaSModelRefs in this namespace
	var models maasv1alpha1.MaaSModelRefList
	if err := r.List(ctx, &models, client.InNamespace(route.Namespace)); err != nil {
		return nil
	}
	if len(models.Items) == 0 {
		return nil
	}
	// Use field indexer to find subscriptions for each model, deduplicating results
	seen := make(map[types.NamespacedName]struct{})
	var requests []reconcile.Request
	for _, m := range models.Items {
		modelKey := m.Namespace + "/" + m.Name
		var subscriptions maasv1alpha1.MaaSSubscriptionList
		if err := r.List(ctx, &subscriptions, client.MatchingFields{modelRefIndexKey: modelKey}); err != nil {
			continue // skip this model on error, don't fail entire mapping
		}
		for _, s := range subscriptions.Items {
			key := types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}
