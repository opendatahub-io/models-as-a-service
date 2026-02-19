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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=tokenratelimitpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch

const maasSubscriptionFinalizer = "maas.opendatahub.io/subscription-cleanup"

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

	// Reconcile TokenRateLimitPolicy for each model
	// IMPORTANT: TokenRateLimitPolicy targets the HTTPRoute for each model
	if err := r.reconcileTokenRateLimitPolicies(ctx, log, subscription); err != nil {
		log.Error(err, "failed to reconcile TokenRateLimitPolicies")
		r.updateStatus(ctx, subscription, "Failed", fmt.Sprintf("Failed to reconcile: %v", err))
		return ctrl.Result{}, err
	}

	r.updateStatus(ctx, subscription, "Active", "Successfully reconciled")
	return ctrl.Result{}, nil
}

func (r *MaaSSubscriptionReconciler) reconcileTokenRateLimitPolicies(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) error {
	// Model-centric approach: for each model referenced by this subscription,
	// find ALL subscriptions for that model and build a single aggregated TRLP.
	// Kuadrant only allows one TokenRateLimitPolicy per HTTPRoute target.
	for _, modelRef := range subscription.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, subscription.Namespace, modelRef.Name)
		if err != nil {
			if errors.Is(err, ErrModelNotFound) {
				log.Info("model not found, cleaning up generated TRLP", "model", modelRef.Name)
				if delErr := r.deleteModelTRLP(ctx, log, modelRef.Name); delErr != nil {
					return fmt.Errorf("failed to clean up TRLP for missing model %s: %w", modelRef.Name, delErr)
				}
				continue
			}
			return fmt.Errorf("failed to resolve HTTPRoute for model %s: %w", modelRef.Name, err)
		}

		// Find ALL subscriptions for this model (not just the current one)
		allSubs, err := findAllSubscriptionsForModel(ctx, r.Client, modelRef.Name)
		if err != nil {
			return fmt.Errorf("failed to list subscriptions for model %s: %w", modelRef.Name, err)
		}

		limitsMap := map[string]interface{}{}
		var allGroupNames, allUserNames []string
		var subNames []string

		type subInfo struct {
			sub        maasv1alpha1.MaaSSubscription
			mRef       maasv1alpha1.ModelSubscriptionRef
			groupNames []string
			userNames  []string
			rates      []interface{}
			maxLimit   int64
		}
		var subs []subInfo
		for _, sub := range allSubs {
			for _, mRef := range sub.Spec.ModelRefs {
				if mRef.Name != modelRef.Name {
					continue
				}
				var groupNames []string
				for _, group := range sub.Spec.Owner.Groups {
					if err := validateCELValue(group.Name, "group name"); err != nil {
						return fmt.Errorf("invalid owner in MaaSSubscription %s: %w", sub.Name, err)
					}
					groupNames = append(groupNames, group.Name)
				}
				var userNames []string
				for _, user := range sub.Spec.Owner.Users {
					if err := validateCELValue(user, "username"); err != nil {
						return fmt.Errorf("invalid owner in MaaSSubscription %s: %w", sub.Name, err)
					}
					userNames = append(userNames, user)
				}
				var rates []interface{}
				var maxLimit int64
				if len(mRef.TokenRateLimits) > 0 {
					for _, trl := range mRef.TokenRateLimits {
						rates = append(rates, map[string]interface{}{"limit": trl.Limit, "window": trl.Window})
						if trl.Limit > maxLimit {
							maxLimit = trl.Limit
						}
					}
				} else {
					rates = append(rates, map[string]interface{}{"limit": int64(100), "window": "1m"})
					maxLimit = 100
				}
				subs = append(subs, subInfo{sub: sub, mRef: mRef, groupNames: groupNames, userNames: userNames, rates: rates, maxLimit: maxLimit})
				break
			}
		}

		// Sort subscriptions by maxLimit descending (highest tier first).
		sort.Slice(subs, func(i, j int) bool { return subs[i].maxLimit > subs[j].maxLimit })

		// Helper: build a compact CEL predicate that checks if the user belongs to
		// any of the given groups or matches any of the given usernames. Uses a single
		// exists() call for groups (e.g. exists(g, g == "a" || g == "b")) instead of
		// N separate exists() calls, keeping predicates short at scale.
		buildMembershipCheck := func(groups, users []string) string {
			var parts []string
			if len(groups) > 0 {
				var comparisons []string
				for _, g := range groups {
					comparisons = append(comparisons, fmt.Sprintf(`g == "%s"`, g))
				}
				parts = append(parts, fmt.Sprintf(`auth.identity.groups_str.split(",").exists(g, %s)`, strings.Join(comparisons, " || ")))
			}
			for _, u := range users {
				parts = append(parts, fmt.Sprintf(`auth.identity.user.username == "%s"`, u))
			}
			return strings.Join(parts, " || ")
		}

		headerCheck := `request.headers["x-maas-subscription"]`
		headerExists := `request.headers.exists(h, h == "x-maas-subscription")`

		for i, si := range subs {
			subNames = append(subNames, si.sub.Name)
			allGroupNames = append(allGroupNames, si.groupNames...)
			allUserNames = append(allUserNames, si.userNames...)

			membershipCheck := buildMembershipCheck(si.groupNames, si.userNames)
			if membershipCheck == "" {
				log.Info("skipping subscription with no owner groups/users — rate limit would be unreachable",
					"subscription", si.sub.Name, "model", si.mRef.Name)
				continue
			}

			// Collect higher-tier groups/users for exclusions
			var excludeGroups, excludeUsers []string
			for j := 0; j < i; j++ {
				excludeGroups = append(excludeGroups, subs[j].groupNames...)
				excludeUsers = append(excludeUsers, subs[j].userNames...)
			}

			// Build branch selection: explicit header OR auto-select with exclusions.
			// These remain OR'd in one predicate because they share the same counter.
			explicitBranch := fmt.Sprintf(`%s == "%s"`, headerCheck, si.sub.Name)
			autoBranch := "!" + headerExists
			if exclusionCheck := buildMembershipCheck(excludeGroups, excludeUsers); exclusionCheck != "" {
				autoBranch += " && !(" + exclusionCheck + ")"
			}

			limitsMap[fmt.Sprintf("%s-%s-tokens", si.sub.Name, si.mRef.Name)] = map[string]interface{}{
				"rates": si.rates,
				"when": []interface{}{
					map[string]interface{}{"predicate": membershipCheck},
					map[string]interface{}{"predicate": explicitBranch + " || (" + autoBranch + ")"},
				},
				"counters": []interface{}{
					map[string]interface{}{"expression": "auth.identity.userid"},
				},
			}
		}

		// Deny-unsubscribed: user is not in ANY subscription group/user list.
		if denyCheck := buildMembershipCheck(allGroupNames, allUserNames); denyCheck != "" {
			limitsMap[fmt.Sprintf("deny-unsubscribed-%s", modelRef.Name)] = map[string]interface{}{
				"rates":    []interface{}{map[string]interface{}{"limit": int64(0), "window": "1m"}},
				"when":     []interface{}{map[string]interface{}{"predicate": "!(" + denyCheck + ")"}},
				"counters": []interface{}{map[string]interface{}{"expression": "auth.identity.userid"}},
			}
		}

		// Deny invalid header: header present but doesn't match any known subscription.
		if len(subNames) > 0 {
			denyHeaderWhen := []interface{}{
				map[string]interface{}{"predicate": headerExists},
			}
			for _, name := range subNames {
				denyHeaderWhen = append(denyHeaderWhen,
					map[string]interface{}{"predicate": fmt.Sprintf(`%s != "%s"`, headerCheck, name)},
				)
			}
			limitsMap[fmt.Sprintf("deny-invalid-header-%s", modelRef.Name)] = map[string]interface{}{
				"rates":    []interface{}{map[string]interface{}{"limit": int64(0), "window": "1m"}},
				"when":     denyHeaderWhen,
				"counters": []interface{}{map[string]interface{}{"expression": "auth.identity.userid"}},
			}
		}

		// Build the aggregated TRLP (one per model, covering all subscriptions)
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

		// Create or update
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(policy.GroupVersionKind())
		err = r.Get(ctx, client.ObjectKeyFromObject(policy), existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, policy); err != nil {
				return fmt.Errorf("failed to create TRLP for model %s: %w", modelRef.Name, err)
			}
			log.Info("TokenRateLimitPolicy created", "name", policyName, "model", modelRef.Name, "subscriptions", subNames)
		} else if err != nil {
			return fmt.Errorf("failed to get existing TRLP: %w", err)
		} else {
			if existing.GetAnnotations()["maas.opendatahub.io/managed"] == "false" {
				log.Info("TokenRateLimitPolicy opted out, skipping", "name", policyName)
			} else {
				mergedAnnotations := existing.GetAnnotations()
				if mergedAnnotations == nil {
					mergedAnnotations = make(map[string]string)
				}
				for k, v := range policy.GetAnnotations() {
					mergedAnnotations[k] = v
				}
				existing.SetAnnotations(mergedAnnotations)
				existing.SetLabels(policy.GetLabels())
				if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
					return fmt.Errorf("failed to update spec: %w", err)
				}
				if err := r.Update(ctx, existing); err != nil {
					return fmt.Errorf("failed to update TRLP for model %s: %w", modelRef.Name, err)
				}
				log.Info("TokenRateLimitPolicy updated", "name", policyName, "model", modelRef.Name, "subscriptions", subNames)
			}
		}
	}
	return nil
}

// deleteModelTRLP deletes the aggregated TRLP for a model by label.
func (r *MaaSSubscriptionReconciler) deleteModelTRLP(ctx context.Context, log logr.Logger, modelName string) error {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-subscription",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		return fmt.Errorf("failed to list TRLPs for cleanup: %w", err)
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		log.Info("Deleting TRLP", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete TRLP %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}

// findHTTPRouteForModel delegates to the shared helper in helpers.go.
func (r *MaaSSubscriptionReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
	return findHTTPRouteForModel(ctx, r.Client, defaultNS, modelName)
}

func (r *MaaSSubscriptionReconciler) handleDeletion(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(subscription, maasSubscriptionFinalizer) {
		for _, modelRef := range subscription.Spec.ModelRefs {
			log.Info("Deleting model TRLP so remaining subscriptions can rebuild it", "model", modelRef.Name)
			if err := r.deleteModelTRLP(ctx, log, modelRef.Name); err != nil {
				log.Error(err, "failed to clean up TRLP, will retry", "model", modelRef.Name)
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

func (r *MaaSSubscriptionReconciler) updateStatus(ctx context.Context, subscription *maasv1alpha1.MaaSSubscription, phase, message string) {
	subscription.Status.Phase = phase
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	if phase == "Failed" {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ReconcileFailed"
	}

	// Update condition
	found := false
	for i, c := range subscription.Status.Conditions {
		if c.Type == condition.Type {
			subscription.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		subscription.Status.Conditions = append(subscription.Status.Conditions, condition)
	}

	if err := r.Status().Update(ctx, subscription); err != nil {
		log := logr.FromContextOrDiscard(ctx)
		log.Error(err, "failed to update MaaSSubscription status", "name", subscription.Name)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSSubscriptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch generated TokenRateLimitPolicies so we re-reconcile when someone manually edits them.
	generatedTRLP := &unstructured.Unstructured{}
	generatedTRLP.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSSubscription{}).
		// Watch HTTPRoutes so we re-reconcile when KServe creates/updates a route
		// (fixes race condition where MaaSSubscription is created before HTTPRoute exists).
		Watches(&gatewayapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(
			r.mapHTTPRouteToMaaSSubscriptions,
		)).
		// Watch MaaSModels so we re-reconcile when a model is created or deleted.
		Watches(&maasv1alpha1.MaaSModel{}, handler.EnqueueRequestsFromMapFunc(
			r.mapMaaSModelToMaaSSubscriptions,
		)).
		// Watch generated TokenRateLimitPolicies so manual edits get overwritten by the controller.
		Watches(generatedTRLP, handler.EnqueueRequestsFromMapFunc(
			r.mapGeneratedTRLPToParent,
		)).
		Complete(r)
}

// mapGeneratedTRLPToParent maps a generated TokenRateLimitPolicy back to any
// MaaSSubscription that references the same model. The TRLP is per-model (aggregated),
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
	sub := findAnySubscriptionForModel(ctx, r.Client, modelName)
	if sub == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: sub.Name, Namespace: sub.Namespace},
	}}
}

// mapMaaSModelToMaaSSubscriptions returns reconcile requests for all MaaSSubscriptions
// that reference the given MaaSModel.
func (r *MaaSSubscriptionReconciler) mapMaaSModelToMaaSSubscriptions(ctx context.Context, obj client.Object) []reconcile.Request {
	model, ok := obj.(*maasv1alpha1.MaaSModel)
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
			if ref.Name == model.Name {
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
// that reference models whose LLMInferenceService lives in the HTTPRoute's namespace.
func (r *MaaSSubscriptionReconciler) mapHTTPRouteToMaaSSubscriptions(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayapiv1.HTTPRoute)
	if !ok {
		return nil
	}
	// Find MaaSModels in this namespace
	var models maasv1alpha1.MaaSModelList
	if err := r.List(ctx, &models); err != nil {
		return nil
	}
	modelNamesInNS := map[string]bool{}
	for _, m := range models.Items {
		ns := m.Spec.ModelRef.Namespace
		if ns == "" {
			ns = m.Namespace
		}
		if ns == route.Namespace {
			modelNamesInNS[m.Name] = true
		}
	}
	if len(modelNamesInNS) == 0 {
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
			if modelNamesInNS[ref.Name] {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: s.Name, Namespace: s.Namespace},
				})
				break
			}
		}
	}
	return requests
}
