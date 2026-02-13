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
	"fmt"
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
	// Create one TokenRateLimitPolicy per model in the subscription
	// Each policy targets the HTTPRoute for that specific model
	for _, modelRef := range subscription.Spec.ModelRefs {
		// Find the MaaSModel to determine HTTPRoute name and namespace
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, subscription.Namespace, modelRef.Name)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				// Model or HTTPRoute genuinely not found (deleted) — skip and clean up
				log.Info("model not found, cleaning up generated policy", "model", modelRef.Name)
				r.deleteGeneratedTRLPForModel(ctx, log, subscription, modelRef.Name)
				continue
			}
			// Transient error (API timeout, RBAC, etc.) — don't delete, requeue
			return fmt.Errorf("failed to resolve HTTPRoute for model %s: %w", modelRef.Name, err)
		}

		policyName := fmt.Sprintf("subscription-%s-model-%s", subscription.Name, modelRef.Name)

		// Use unstructured for TokenRateLimitPolicy since Go types may not be available
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "kuadrant.io",
			Version: "v1alpha1",
			Kind:    "TokenRateLimitPolicy",
		})
		policy.SetName(policyName)
		// Use the same namespace as the HTTPRoute
		policy.SetNamespace(httpRouteNS)

		// Set labels to link TokenRateLimitPolicy to Subscription
		// This helps OpenShift UI show the relationship in "resource of" tab
		labels := map[string]string{
			"maas.opendatahub.io/subscription":    subscription.Name,
			"maas.opendatahub.io/subscription-ns": subscription.Namespace,
			"maas.opendatahub.io/model":           modelRef.Name,
			"app.kubernetes.io/managed-by":        "maas-controller",
			"app.kubernetes.io/part-of":           "maas-subscription",
			"app.kubernetes.io/component":         "token-rate-limit-policy",
		}
		policy.SetLabels(labels)

		// Set owner reference (only if in same namespace - Kubernetes doesn't allow cross-namespace owner references)
		// For cross-namespace, labels above will help OpenShift UI show the relationship in "resource of" tab
		if httpRouteNS == subscription.Namespace {
			if err := controllerutil.SetControllerReference(subscription, policy, r.Scheme); err != nil {
				return fmt.Errorf("failed to set controller reference: %w", err)
			}
		}
		// Note: For cross-namespace resources, we rely on labels to establish the relationship
		// OpenShift UI can use these labels to show TokenRateLimitPolicy in the Subscription's "resource of" tab

		// Build limits from subscription spec (structure matches example-tokenratepolicy.yaml)
		limitKey := fmt.Sprintf("%s-%s-tokens", subscription.Name, modelRef.Name)

		// Convert token rate limits to []interface{}; use default rate if none specified
		var rates []interface{}
		if len(modelRef.TokenRateLimits) > 0 {
			for _, trl := range modelRef.TokenRateLimits {
				rates = append(rates, map[string]interface{}{
					"limit":  trl.Limit,
					"window": trl.Window,
				})
			}
		} else {
			rates = append(rates, map[string]interface{}{
				"limit":  int64(100),
				"window": "1m",
			})
		}

		// Build predicate so the limit applies and 429s can occur:
		// - Exclude /v1/models (list models) from counting toward the limit.
		// - When subscription has Owner (groups/users): apply limit when user is in one of those groups
		//   or is one of those users. Use auth.identity.user.groups (TokenReview) and optionally
		//   auth.identity.groups if set by AuthPolicy response. Do not require X-MAAS-HEADER so
		//   curl with Bearer token alone can trigger the limit.
		// - When no owner: require X-MAAS-HEADER so only requests with that header are limited.

		pathCheck := `!request.path.endsWith("/v1/models")`

		var groupChecks []string
		for _, group := range subscription.Spec.Owner.Groups {
			// Use comma-separated groups_str from AuthPolicy response: auth.identity.groups_str.split(",").exists(g, g == "group")
			groupChecks = append(groupChecks, fmt.Sprintf(`auth.identity.groups_str.split(",").exists(g, g == "%s")`, group.Name))
		}

		var userChecks []string
		for _, user := range subscription.Spec.Owner.Users {
			userChecks = append(userChecks, fmt.Sprintf(`auth.identity.user.username == "%s"`, user))
		}

		var ownerChecks []string
		ownerChecks = append(ownerChecks, groupChecks...)
		ownerChecks = append(ownerChecks, userChecks...)

		var predicate string
		if len(ownerChecks) > 0 {
			ownerPredicate := "(" + ownerChecks[0]
			for i := 1; i < len(ownerChecks); i++ {
				ownerPredicate += " || " + ownerChecks[i]
			}
			ownerPredicate += ")"
			// Apply limit when path is not /v1/models and user matches owner (no header required).
			predicate = pathCheck + " && " + ownerPredicate
		} else {
			// No owner: require X-MAAS-HEADER so only subscription-scoped requests are limited.
			subscriptionIDCheck := fmt.Sprintf(`request.http.headers["X-MAAS-HEADER"] == "%s"`, subscription.Name)
			predicate = pathCheck + " && " + subscriptionIDCheck
		}

		// Build the limits map with the subscription's rate limit rule
		limitsMap := map[string]interface{}{
			limitKey: map[string]interface{}{
				"rates": rates,
				"when": []interface{}{
					map[string]interface{}{
						"predicate": predicate,
					},
				},
				"counters": []interface{}{
					map[string]interface{}{
						"expression": "auth.identity.userid",
					},
				},
			},
		}

		// Add deny-unsubscribed catch-all rule.
		// When a per-route TRLP exists, it completely overrides the gateway-level
		// default-deny (Kuadrant atomic defaults strategy). Without this catch-all,
		// users who don't match the subscription's group/user predicates would get
		// NO rate limit at all (effectively unlimited access).
		// This rule ensures those users still get 429 (0 tokens).
		if len(ownerChecks) > 0 {
			denyOwnerPredicate := "!(" + strings.Join(ownerChecks, " || ") + ")"
			denyPredicate := pathCheck + " && " + denyOwnerPredicate
			denyLimitKey := fmt.Sprintf("%s-%s-deny-unsubscribed", subscription.Name, modelRef.Name)
			limitsMap[denyLimitKey] = map[string]interface{}{
				"rates": []interface{}{
					map[string]interface{}{
						"limit":  int64(0),
						"window": "1m",
					},
				},
				"when": []interface{}{
					map[string]interface{}{
						"predicate": denyPredicate,
					},
				},
				"counters": []interface{}{
					map[string]interface{}{
						"expression": "auth.identity.userid",
					},
				},
			}
		}

		// Build the spec - target the HTTPRoute
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

		// Add billing metadata as annotations
		if subscription.Spec.BillingMetadata != nil {
			annotations := make(map[string]string)
			if subscription.Spec.BillingMetadata.OrganizationID != "" {
				annotations["maas.opendatahub.io/organization-id"] = subscription.Spec.BillingMetadata.OrganizationID
			}
			if subscription.Spec.BillingMetadata.CostCenter != "" {
				annotations["maas.opendatahub.io/cost-center"] = subscription.Spec.BillingMetadata.CostCenter
			}
			for k, v := range subscription.Spec.BillingMetadata.Labels {
				annotations[fmt.Sprintf("maas.opendatahub.io/label/%s", k)] = v
			}
			policy.SetAnnotations(annotations)
		}

		// Create or update the policy
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(policy.GroupVersionKind())
		key := client.ObjectKeyFromObject(policy)

		err = r.Get(ctx, key, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, policy); err != nil {
				return fmt.Errorf("failed to create TokenRateLimitPolicy for model %s: %w", modelRef.Name, err)
			}
			log.Info("TokenRateLimitPolicy created", "name", policyName, "model", modelRef.Name, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		} else if err != nil {
			return fmt.Errorf("failed to get existing TokenRateLimitPolicy: %w", err)
		} else {
			// Skip update if the generated policy has opted out of management
			if existing.GetAnnotations()["maas.opendatahub.io/managed"] == "false" {
				log.Info("TokenRateLimitPolicy has maas.opendatahub.io/managed=false, skipping update", "name", policyName, "model", modelRef.Name)
			} else {
				// Merge annotations to preserve existing ones (e.g. from Kuadrant or user)
				mergedAnnotations := existing.GetAnnotations()
				if mergedAnnotations == nil {
					mergedAnnotations = make(map[string]string)
				}
				for k, v := range policy.GetAnnotations() {
					mergedAnnotations[k] = v
				}
				existing.SetAnnotations(mergedAnnotations)
				existing.SetLabels(policy.GetLabels())
				// Update owner references if in same namespace
				if httpRouteNS == subscription.Namespace {
					if err := controllerutil.SetControllerReference(subscription, existing, r.Scheme); err != nil {
						return fmt.Errorf("failed to update controller reference: %w", err)
					}
				}
				if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
					return fmt.Errorf("failed to update spec: %w", err)
				}
				if err := r.Update(ctx, existing); err != nil {
					return fmt.Errorf("failed to update TokenRateLimitPolicy for model %s: %w", modelRef.Name, err)
				}
				log.Info("TokenRateLimitPolicy updated", "name", policyName, "model", modelRef.Name, "httpRoute", httpRouteName, "namespace", httpRouteNS)
			}
		}
	}

	return nil
}

// deleteGeneratedTRLPForModel deletes the generated TokenRateLimitPolicy for a specific model
// that no longer exists (e.g. MaaSModel was deleted).
func (r *MaaSSubscriptionReconciler) deleteGeneratedTRLPForModel(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription, modelName string) {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":        modelName,
		"maas.opendatahub.io/subscription": subscription.Name,
		"app.kubernetes.io/managed-by":     "maas-controller",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		log.Error(err, "failed to list TokenRateLimitPolicies for cleanup", "model", modelName)
		return
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		log.Info("Deleting orphaned TokenRateLimitPolicy for deleted model", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete orphaned TokenRateLimitPolicy", "name", p.GetName())
		}
	}
}

// findHTTPRouteForModel finds the HTTPRoute for a given model name
// It searches for MaaSModel resources and determines the HTTPRoute name based on the model kind
func (r *MaaSSubscriptionReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
	// List all MaaSModels and find the one with matching name
	maasModelList := &maasv1alpha1.MaaSModelList{}
	if err := r.List(ctx, maasModelList); err != nil {
		return "", "", fmt.Errorf("failed to list MaaSModels: %w", err)
	}

	// Find matching MaaSModel (try defaultNS first, then any namespace)
	var maasModel *maasv1alpha1.MaaSModel
	for i := range maasModelList.Items {
		if maasModelList.Items[i].Name == modelName {
			// Skip models that are being deleted
			if !maasModelList.Items[i].GetDeletionTimestamp().IsZero() {
				continue
			}
			// Prefer the one in defaultNS if it exists
			if maasModelList.Items[i].Namespace == defaultNS {
				maasModel = &maasModelList.Items[i]
				break
			}
			// Otherwise, use the first match
			if maasModel == nil {
				maasModel = &maasModelList.Items[i]
			}
		}
	}

	if maasModel == nil {
		return "", "", fmt.Errorf("MaaSModel %s not found", modelName)
	}

	// Determine HTTPRoute name and namespace based on model kind
	var httpRouteName string
	// For HTTPRoute namespace, use ModelRef.Namespace if specified, otherwise use the namespace where the model resource exists
	// For llmisvc, the HTTPRoute is in the same namespace as the LLMInferenceService
	httpRouteNS := maasModel.Namespace
	if maasModel.Spec.ModelRef.Namespace != "" {
		httpRouteNS = maasModel.Spec.ModelRef.Namespace
	}

	switch maasModel.Spec.ModelRef.Kind {
	case "llmisvc":
		// For llmisvc, find HTTPRoute using labels
		// The HTTPRoute is in the same namespace as the LLMInferenceService
		// Use ModelRef.Namespace if specified, otherwise use the namespace where the LLMInferenceService exists
		llmisvcNS := maasModel.Namespace
		if maasModel.Spec.ModelRef.Namespace != "" {
			llmisvcNS = maasModel.Spec.ModelRef.Namespace
		}

		routeList := &gatewayapiv1.HTTPRouteList{}
		labelSelector := client.MatchingLabels{
			"app.kubernetes.io/name":      maasModel.Spec.ModelRef.Name,
			"app.kubernetes.io/component": "llminferenceservice-router",
			"app.kubernetes.io/part-of":   "llminferenceservice",
		}

		if err := r.List(ctx, routeList, client.InNamespace(llmisvcNS), labelSelector); err != nil {
			return "", "", fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", maasModel.Spec.ModelRef.Name, err)
		}

		if len(routeList.Items) == 0 {
			return "", "", fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", maasModel.Spec.ModelRef.Name, llmisvcNS)
		}

		httpRouteName = routeList.Items[0].Name
		// HTTPRoute namespace is where we actually found it (use the HTTPRoute's namespace)
		httpRouteNS = routeList.Items[0].Namespace
	case "ExternalModel":
		// For ExternalModel, use the MaaSModel HTTPRoute naming convention
		httpRouteName = fmt.Sprintf("maas-model-%s", maasModel.Name)
	default:
		return "", "", fmt.Errorf("unknown model kind: %s", maasModel.Spec.ModelRef.Kind)
	}

	// Verify the HTTPRoute exists
	httpRoute := &gatewayapiv1.HTTPRoute{}
	key := client.ObjectKey{
		Name:      httpRouteName,
		Namespace: httpRouteNS,
	}
	if err := r.Get(ctx, key, httpRoute); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("HTTPRoute %s/%s not found for model %s", httpRouteNS, httpRouteName, modelName)
		}
		return "", "", fmt.Errorf("failed to get HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}

	return httpRouteName, httpRouteNS, nil
}

func (r *MaaSSubscriptionReconciler) handleDeletion(ctx context.Context, log logr.Logger, subscription *maasv1alpha1.MaaSSubscription) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(subscription, maasSubscriptionFinalizer) {
		// Clean up all TokenRateLimitPolicies for this subscription
		for _, modelRef := range subscription.Spec.ModelRefs {
			_, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, subscription.Namespace, modelRef.Name)
			if err != nil {
				log.Info("failed to find HTTPRoute for model during deletion, trying subscription namespace", "model", modelRef.Name, "error", err)
				httpRouteNS = subscription.Namespace
			}

			policyName := fmt.Sprintf("subscription-%s-model-%s", subscription.Name, modelRef.Name)
			policy := &unstructured.Unstructured{}
			policy.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kuadrant.io",
				Version: "v1alpha1",
				Kind:    "TokenRateLimitPolicy",
			})
			policy.SetName(policyName)
			policy.SetNamespace(httpRouteNS)

			if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete TokenRateLimitPolicy", "name", policyName, "namespace", httpRouteNS)
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

// mapGeneratedTRLPToParent maps a generated TokenRateLimitPolicy back to its parent
// MaaSSubscription using the labels set at creation time.
func (r *MaaSSubscriptionReconciler) mapGeneratedTRLPToParent(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "maas-controller" {
		return nil // not a generated policy
	}
	subName := labels["maas.opendatahub.io/subscription"]
	subNS := labels["maas.opendatahub.io/subscription-ns"]
	if subName == "" || subNS == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: subName, Namespace: subNS},
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
		if m.Spec.ModelRef.Namespace == route.Namespace {
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
