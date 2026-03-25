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
	"sort"
	"strings"

	"github.com/go-logr/logr"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
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

const maasSubscriptionFinalizer = "maas.opendatahub.io/subscription-cleanup"

// ConditionSpecPriorityDuplicate is set True when another MaaSSubscription shares the same spec.priority
// (API key mint and selector use deterministic tie-break; admins should set distinct priorities).
const ConditionSpecPriorityDuplicate = "SpecPriorityDuplicate"

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

	// Reconcile TokenRateLimitPolicy for each model
	// IMPORTANT: TokenRateLimitPolicy targets the HTTPRoute for each model
	if err := r.reconcileTokenRateLimitPolicies(ctx, log, subscription); err != nil {
		log.Error(err, "failed to reconcile TokenRateLimitPolicies")
		r.updateStatus(ctx, subscription, "Failed", fmt.Sprintf("Failed to reconcile: %v", err), statusSnapshot)
		return ctrl.Result{}, err
	}

	r.updateStatus(ctx, subscription, "Active", "Successfully reconciled", statusSnapshot)
	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) reconcileTokenRateLimitPolicies(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) error {
	// Model-centric approach: for each model referenced by this subscription,
	// find ALL subscriptions for that model and build a single aggregated TokenRateLimitPolicy.
	// Kuadrant only allows one TokenRateLimitPolicy per HTTPRoute target.
	for _, modelRef := range subscription.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, modelRef.Namespace, modelRef.Name)
		if err != nil {
			if errors.Is(err, ErrModelNotFound) {
				log.Info("model not found, cleaning up generated TokenRateLimitPolicy", "model", modelRef.Namespace+"/"+modelRef.Name)
				if delErr := r.deleteModelTRLP(ctx, log, modelRef.Namespace, modelRef.Name); delErr != nil {
					return fmt.Errorf("failed to clean up TokenRateLimitPolicy for missing model %s/%s: %w", modelRef.Namespace, modelRef.Name, delErr)
				}
				continue
			}
			return fmt.Errorf("failed to resolve HTTPRoute for model %s/%s: %w", modelRef.Namespace, modelRef.Name, err)
		}

		// Find ALL subscriptions for this model (not just the current one)
		allSubs, err := findAllSubscriptionsForModel(ctx, r.Client, modelRef.Namespace, modelRef.Name)
		if err != nil {
			return fmt.Errorf("failed to list subscriptions for model %s/%s: %w", modelRef.Namespace, modelRef.Name, err)
		}

		limitsMap := map[string]interface{}{}
		var subNames []string

		type subInfo struct {
			sub   maasv1alpha1.MaaSSubscription
			mRef  maasv1alpha1.ModelSubscriptionRef
			rates []interface{}
		}
		var subs []subInfo
		for _, sub := range allSubs {
			for _, mRef := range sub.Spec.ModelRefs {
				if mRef.Namespace != modelRef.Namespace || mRef.Name != modelRef.Name {
					continue
				}
				var rates []interface{}
				if len(mRef.TokenRateLimits) > 0 {
					for _, trl := range mRef.TokenRateLimits {
						rates = append(rates, map[string]interface{}{"limit": trl.Limit, "window": trl.Window})
					}
				} else {
					rates = append(rates, map[string]interface{}{"limit": int64(100), "window": "1m"})
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
			limitsMap[fmt.Sprintf("%s-%s-tokens", safeKey, si.mRef.Name)] = map[string]interface{}{
				"rates": si.rates,
				"when": []interface{}{
					map[string]interface{}{
						"predicate": fmt.Sprintf(`auth.identity.selected_subscription_key == "%s"`, modelScopedRef),
					},
				},
				"counters": []interface{}{
					map[string]interface{}{"expression": "auth.identity.userid"},
				},
			}
		}

		// Sort subscription names for stable annotation value across reconciles
		sort.Strings(subNames)

		// Build the aggregated TokenRateLimitPolicy (one per model, covering all subscriptions)
		policyName := fmt.Sprintf("maas-trlp-%s", modelRef.Name)
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
		policy.SetName(policyName)
		policy.SetNamespace(httpRouteNS)
		policy.SetLabels(map[string]string{
			"maas.opendatahub.io/model":    modelRef.Name,
			"app.kubernetes.io/managed-by": "maas-controller",
			"app.kubernetes.io/part-of":    "maas-subscription",
			"app.kubernetes.io/component":  "token-rate-limit-policy",
		})
		policy.SetAnnotations(map[string]string{
			"maas.opendatahub.io/subscriptions": strings.Join(subNames, ","),
		})

		spec := map[string]interface{}{
			"targetRef": map[string]interface{}{
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
				return fmt.Errorf("failed to create TokenRateLimitPolicy for model %s: %w", modelRef.Name, err)
			}
			log.Info("TokenRateLimitPolicy created", "name", policyName, "model", modelRef.Name, "subscriptionCount", len(subNames), "subscriptions", subNames)
		} else if err != nil {
			return fmt.Errorf("failed to get existing TokenRateLimitPolicy: %w", err)
		} else {
			if !isManaged(existing) {
				log.Info("TokenRateLimitPolicy opted out, skipping", "name", policyName)
			} else {
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
					log.Info("TokenRateLimitPolicy unchanged, skipping update", "name", policyName, "model", modelRef.Namespace+"/"+modelRef.Name, "subscriptionCount", len(subNames))
				} else {
					if err := r.Update(ctx, existing); err != nil {
						return fmt.Errorf("failed to update TokenRateLimitPolicy for model %s/%s: %w", modelRef.Namespace, modelRef.Name, err)
					}
					log.Info("TokenRateLimitPolicy updated", "name", policyName, "model", modelRef.Namespace+"/"+modelRef.Name, "subscriptionCount", len(subNames), "subscriptions", subNames)
				}
			}
		}
	}
	return nil
}

// deleteModelTRLP deletes the aggregated TokenRateLimitPolicy for a model in the given namespace.
func (r *MaaSSubscriptionReconciler) deleteModelTRLP(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	// Always delete the aggregated TokenRateLimitPolicy so remaining MaaSSubscriptions rebuild it
	// without the rate limits from the deleted subscription. If we skip deletion, the aggregated
	// TokenRateLimitPolicy will contain stale configuration from the deleted MaaSSubscription.
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-subscription",
	}
	if err := r.List(ctx, policyList, client.InNamespace(modelNamespace), labelSelector); err != nil {
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
		for _, modelRef := range subscription.Spec.ModelRefs {
			log.Info("Deleting model TokenRateLimitPolicy so remaining subscriptions can rebuild it", "model", modelRef.Namespace+"/"+modelRef.Name)
			if err := r.deleteModelTRLP(ctx, log, modelRef.Namespace, modelRef.Name); err != nil {
				log.Error(err, "failed to clean up TokenRateLimitPolicy, will retry", "model", modelRef.Namespace+"/"+modelRef.Name)
				return ctrl.Result{}, err
			}
		}

		controllerutil.RemoveFinalizer(subscription, maasSubscriptionFinalizer)
		if err := r.Update(ctx, subscription); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) updateStatus(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription, phase, message string, statusSnapshot *maasv1alpha1.MaaSSubscriptionStatus) {
	// Status-only updates do not bump metadata.generation, so this reconcile may not re-queue.
	// Merge SpecPriorityDuplicate from the API server so we do not clobber the async duplicate-priority scan.
	latest := &maasv1alpha1.MaaSSubscription{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(subscription), latest); err == nil {
		if dup := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionSpecPriorityDuplicate); dup != nil {
			apimeta.SetStatusCondition(&subscription.Status.Conditions, *dup)
		}
	}

	subscription.Status.Phase = phase

	status := metav1.ConditionTrue
	reason := "Reconciled"
	if phase == "Failed" {
		status = metav1.ConditionFalse
		reason = "ReconcileFailed"
	}

	apimeta.SetStatusCondition(&subscription.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
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
	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, s := range subscriptions.Items {
		for _, ref := range s.Spec.ModelRefs {
			if ref.Namespace == model.Namespace && ref.Name == model.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: s.Name, Namespace: s.Namespace},
				})
				break
			}
		}
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
	// Use namespace-qualified keys to prevent cross-namespace matches
	modelKeysInNS := map[string]bool{}
	for _, m := range models.Items {
		modelKeysInNS[m.Namespace+"/"+m.Name] = true
	}
	if len(modelKeysInNS) == 0 {
		return nil
	}
	// Find MaaSSubscriptions that reference any of these models
	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, s := range subscriptions.Items {
		for _, ref := range s.Spec.ModelRefs {
			if modelKeysInNS[ref.Namespace+"/"+ref.Name] {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: s.Name, Namespace: s.Namespace},
				})
				break
			}
		}
	}
	return requests
}
