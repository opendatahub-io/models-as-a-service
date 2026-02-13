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

// MaaSAuthPolicyReconciler reconciles a MaaSAuthPolicy object
type MaaSAuthPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasauthpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop
const maasAuthPolicyFinalizer = "maas.opendatahub.io/authpolicy-cleanup"

func (r *MaaSAuthPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSAuthPolicy", req.NamespacedName)

	policy := &maasv1alpha1.MaaSAuthPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSAuthPolicy")
		return ctrl.Result{}, err
	}

	if !policy.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, policy)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		controllerutil.AddFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	refs, err := r.reconcileModelAuthPolicies(ctx, log, policy)
	if err != nil {
		log.Error(err, "failed to reconcile model AuthPolicies")
		r.updateStatus(ctx, policy, "Failed", fmt.Sprintf("Failed to reconcile: %v", err))
		return ctrl.Result{}, err
	}

	r.updateAuthPolicyRefStatus(ctx, log, policy, refs)
	r.updateStatus(ctx, policy, "Active", "Successfully reconciled")
	return ctrl.Result{}, nil
}

type authPolicyRef struct {
	Name      string
	Namespace string
	Model     string
}

func (r *MaaSAuthPolicyReconciler) reconcileModelAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) ([]authPolicyRef, error) {
	var refs []authPolicyRef
	for _, modelName := range policy.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, policy.Namespace, modelName)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				// Model or HTTPRoute genuinely not found (deleted) — skip and clean up
				log.Info("model not found, cleaning up generated policy", "model", modelName)
				r.deleteGeneratedAuthPolicyForModel(ctx, log, policy, modelName)
				continue
			}
			// Transient error (API timeout, RBAC, etc.) — don't delete, requeue
			return nil, fmt.Errorf("failed to resolve HTTPRoute for model %s: %w", modelName, err)
		}
		refs = append(refs, authPolicyRef{
			Name:      fmt.Sprintf("maas-auth-%s-model-%s", policy.Name, modelName),
			Namespace: httpRouteNS,
			Model:     modelName,
		})

		authPolicyName := fmt.Sprintf("maas-auth-%s-model-%s", policy.Name, modelName)
		authPolicy := &unstructured.Unstructured{}
		authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		authPolicy.SetName(authPolicyName)
		authPolicy.SetNamespace(httpRouteNS)

		labels := map[string]string{
			"maas.opendatahub.io/auth-policy":    policy.Name,
			"maas.opendatahub.io/auth-policy-ns": policy.Namespace,
			"maas.opendatahub.io/model":          modelName,
			"app.kubernetes.io/managed-by":       "maas-controller",
			"app.kubernetes.io/part-of":          "maas-auth-policy",
			"app.kubernetes.io/component":        "auth-policy",
		}
		authPolicy.SetLabels(labels)

		if httpRouteNS == policy.Namespace {
			if err := controllerutil.SetControllerReference(policy, authPolicy, r.Scheme); err != nil {
				return nil, fmt.Errorf("failed to set controller reference: %w", err)
			}
		}

		var groupNames []string
		for _, group := range policy.Spec.Subjects.Groups {
			groupNames = append(groupNames, group.Name)
		}
		audiences := []interface{}{"maas-default-gateway-sa", "https://kubernetes.default.svc"}

		rule := map[string]interface{}{
			"authentication": map[string]interface{}{
				"service-accounts": map[string]interface{}{
					"cache": map[string]interface{}{
						"key": map[string]interface{}{"selector": "context.request.http.headers.authorization.@case:lower"},
						"ttl": int64(600),
					},
					"defaults": map[string]interface{}{
						"userid": map[string]interface{}{"selector": "auth.identity.user.username"},
					},
					"kubernetesTokenReview": map[string]interface{}{"audiences": audiences},
					"metrics":               false, "priority": int64(0),
				},
			},
		}

		// Build authorization rule: user must be in at least one allowed group or be a named user.
		// Uses positive "incl" check instead of the old "excl + empty-value deny" approach,
		// which broke because Kuadrant strips value: "" when converting AuthPolicy to AuthConfig.
		var membershipConditions []interface{}
		for _, g := range groupNames {
			membershipConditions = append(membershipConditions, map[string]interface{}{
				"operator": "incl", "selector": "auth.identity.user.groups", "value": g,
			})
		}
		for _, user := range policy.Spec.Subjects.Users {
			membershipConditions = append(membershipConditions, map[string]interface{}{
				"operator": "eq", "selector": "auth.identity.user.username", "value": user,
			})
		}

		if len(membershipConditions) > 0 {
			// Single condition: use directly. Multiple conditions: wrap in "any" for OR semantics
			// (user must match at least one group or username). If none match → 403.
			var patterns []interface{}
			if len(membershipConditions) == 1 {
				patterns = membershipConditions
			} else {
				patterns = []interface{}{map[string]interface{}{"any": membershipConditions}}
			}
			rule["authorization"] = map[string]interface{}{
				"require-group-membership": map[string]interface{}{
					"metrics":  false,
					"priority": int64(0),
					"patternMatching": map[string]interface{}{
						"patterns": patterns,
					},
				},
			}
		}
		// If no groups/users specified, skip authorization — all authenticated users are allowed.
		// Access control is still enforced by the TRLP deny-unsubscribed catch-all rule (429).

		groupsFilterExpr := "auth.identity.user.groups"
		if len(groupNames) > 0 {
			groupsFilterExpr = "auth.identity.user.groups.filter(g, " + groupOrExpr(groupNames) + ")"
		}
		// groups_str: comma-separated list so TokenRateLimitPolicy can use auth.identity.groups_str.split(",").exists(g, g == "group")
		groupsStrExpr := groupsFilterExpr + `.join(",")`
		rule["response"] = map[string]interface{}{
			"success": map[string]interface{}{
				"filters": map[string]interface{}{
					"identity": map[string]interface{}{
						"json": map[string]interface{}{
							"properties": map[string]interface{}{
								"groups":     map[string]interface{}{"expression": groupsFilterExpr},
								"groups_str": map[string]interface{}{"expression": groupsStrExpr},
								"userid": map[string]interface{}{
									"expression": "auth.identity.user.username", "selector": "auth.identity.userid",
								},
							},
						},
						"metrics": true, "priority": int64(0),
					},
				},
			},
		}
		spec := map[string]interface{}{
			"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": httpRouteName},
			"rules":     rule,
		}
		if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}

		if policy.Spec.MeteringMetadata != nil {
			annotations := make(map[string]string)
			if policy.Spec.MeteringMetadata.OrganizationID != "" {
				annotations["maas.opendatahub.io/organization-id"] = policy.Spec.MeteringMetadata.OrganizationID
			}
			if policy.Spec.MeteringMetadata.CostCenter != "" {
				annotations["maas.opendatahub.io/cost-center"] = policy.Spec.MeteringMetadata.CostCenter
			}
			for k, v := range policy.Spec.MeteringMetadata.Labels {
				annotations[fmt.Sprintf("maas.opendatahub.io/label/%s", k)] = v
			}
			authPolicy.SetAnnotations(annotations)
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
		key := client.ObjectKeyFromObject(authPolicy)
		err = r.Get(ctx, key, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, authPolicy); err != nil {
				return nil, fmt.Errorf("failed to create AuthPolicy for model %s: %w", modelName, err)
			}
			log.Info("AuthPolicy created", "name", authPolicyName, "model", modelName, "httpRoute", httpRouteName, "namespace", httpRouteNS)
		} else if err != nil {
			return nil, fmt.Errorf("failed to get existing AuthPolicy: %w", err)
		} else {
			// Skip update if the generated policy has opted out of management
			if existing.GetAnnotations()["maas.opendatahub.io/managed"] == "false" {
				log.Info("AuthPolicy has maas.opendatahub.io/managed=false, skipping update", "name", authPolicyName, "model", modelName)
			} else {
				// Merge annotations to preserve existing ones (e.g. from Kuadrant or user)
				mergedAnnotations := existing.GetAnnotations()
				if mergedAnnotations == nil {
					mergedAnnotations = make(map[string]string)
				}
				for k, v := range authPolicy.GetAnnotations() {
					mergedAnnotations[k] = v
				}
				existing.SetAnnotations(mergedAnnotations)
				existing.SetLabels(authPolicy.GetLabels())
				if httpRouteNS == policy.Namespace {
					if err := controllerutil.SetControllerReference(policy, existing, r.Scheme); err != nil {
						return nil, fmt.Errorf("failed to update controller reference: %w", err)
					}
				}
				if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
					return nil, fmt.Errorf("failed to update spec: %w", err)
				}
				if err := r.Update(ctx, existing); err != nil {
					return nil, fmt.Errorf("failed to update AuthPolicy for model %s: %w", modelName, err)
				}
				log.Info("AuthPolicy updated", "name", authPolicyName, "model", modelName, "httpRoute", httpRouteName, "namespace", httpRouteNS)
			}
		}
	}
	return refs, nil
}

func groupOrExpr(groupNames []string) string {
	parts := make([]string, len(groupNames))
	for i, g := range groupNames {
		parts[i] = fmt.Sprintf(`g == %q`, g)
	}
	return strings.Join(parts, " || ")
}

func (r *MaaSAuthPolicyReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
	maasModelList := &maasv1alpha1.MaaSModelList{}
	if err := r.List(ctx, maasModelList); err != nil {
		return "", "", fmt.Errorf("failed to list MaaSModels: %w", err)
	}
	var maasModel *maasv1alpha1.MaaSModel
	for i := range maasModelList.Items {
		if maasModelList.Items[i].Name == modelName {
			// Skip models that are being deleted
			if !maasModelList.Items[i].GetDeletionTimestamp().IsZero() {
				continue
			}
			if maasModelList.Items[i].Namespace == defaultNS {
				maasModel = &maasModelList.Items[i]
				break
			}
			if maasModel == nil {
				maasModel = &maasModelList.Items[i]
			}
		}
	}
	if maasModel == nil {
		return "", "", fmt.Errorf("MaaSModel %s not found", modelName)
	}
	var httpRouteName string
	httpRouteNS := maasModel.Namespace
	if maasModel.Spec.ModelRef.Namespace != "" {
		httpRouteNS = maasModel.Spec.ModelRef.Namespace
	}
	switch maasModel.Spec.ModelRef.Kind {
	case "llmisvc":
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
		httpRouteNS = routeList.Items[0].Namespace
	case "ExternalModel":
		httpRouteName = fmt.Sprintf("maas-model-%s", maasModel.Name)
	default:
		return "", "", fmt.Errorf("unknown model kind: %s", maasModel.Spec.ModelRef.Kind)
	}
	httpRoute := &gatewayapiv1.HTTPRoute{}
	key := client.ObjectKey{Name: httpRouteName, Namespace: httpRouteNS}
	if err := r.Get(ctx, key, httpRoute); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("HTTPRoute %s/%s not found for model %s", httpRouteNS, httpRouteName, modelName)
		}
		return "", "", fmt.Errorf("failed to get HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}
	return httpRouteName, httpRouteNS, nil
}

// deleteGeneratedAuthPolicyForModel deletes the generated AuthPolicy for a specific model
// that no longer exists (e.g. MaaSModel was deleted).
func (r *MaaSAuthPolicyReconciler) deleteGeneratedAuthPolicyForModel(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy, modelName string) {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":      modelName,
		"maas.opendatahub.io/auth-policy": policy.Name,
		"app.kubernetes.io/managed-by":   "maas-controller",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		log.Error(err, "failed to list AuthPolicies for cleanup", "model", modelName)
		return
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		log.Info("Deleting orphaned AuthPolicy for deleted model", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete orphaned AuthPolicy", "name", p.GetName())
		}
	}
}

func (r *MaaSAuthPolicyReconciler) handleDeletion(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		for _, modelName := range policy.Spec.ModelRefs {
			_, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, policy.Namespace, modelName)
			if err != nil {
				log.Info("failed to find HTTPRoute for model during deletion, trying policy namespace", "model", modelName, "error", err)
				httpRouteNS = policy.Namespace
			}
			authPolicyName := fmt.Sprintf("maas-auth-%s-model-%s", policy.Name, modelName)
			authPolicy := &unstructured.Unstructured{}
			authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
			authPolicy.SetName(authPolicyName)
			authPolicy.SetNamespace(httpRouteNS)
			if err := r.Delete(ctx, authPolicy); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete AuthPolicy", "name", authPolicyName, "namespace", httpRouteNS)
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(policy, maasAuthPolicyFinalizer)
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *MaaSAuthPolicyReconciler) updateAuthPolicyRefStatus(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy, refs []authPolicyRef) {
	policy.Status.AuthPolicies = make([]maasv1alpha1.AuthPolicyRefStatus, 0, len(refs))
	for _, ref := range refs {
		ap := &unstructured.Unstructured{}
		ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		ap.SetNamespace(ref.Namespace)
		ap.SetName(ref.Name)
		if err := r.Get(ctx, client.ObjectKeyFromObject(ap), ap); err != nil {
			log.Info("could not get AuthPolicy for status", "name", ref.Name, "namespace", ref.Namespace, "error", err)
			policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, maasv1alpha1.AuthPolicyRefStatus{
				Name: ref.Name, Namespace: ref.Namespace, Model: ref.Model, Accepted: "Unknown", Enforced: "Unknown",
			})
			continue
		}
		accepted, enforced := getAuthPolicyConditionState(ap)
		policy.Status.AuthPolicies = append(policy.Status.AuthPolicies, maasv1alpha1.AuthPolicyRefStatus{
			Name: ref.Name, Namespace: ref.Namespace, Model: ref.Model, Accepted: accepted, Enforced: enforced,
		})
	}
}

func getAuthPolicyConditionState(ap *unstructured.Unstructured) (accepted, enforced string) {
	accepted, enforced = "Unknown", "Unknown"
	conditions, found, err := unstructured.NestedSlice(ap.Object, "status", "conditions")
	if err != nil || !found || len(conditions) == 0 {
		return accepted, enforced
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		switch typ {
		case "Accepted":
			accepted = status
		case "Enforced":
			enforced = status
		}
	}
	return accepted, enforced
}

func (r *MaaSAuthPolicyReconciler) updateStatus(ctx context.Context, policy *maasv1alpha1.MaaSAuthPolicy, phase, message string) {
	policy.Status.Phase = phase
	condition := metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: message, LastTransitionTime: metav1.Now(),
	}
	if phase == "Failed" {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ReconcileFailed"
	}
	found := false
	for i, c := range policy.Status.Conditions {
		if c.Type == condition.Type {
			policy.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		policy.Status.Conditions = append(policy.Status.Conditions, condition)
	}
	if err := r.Status().Update(ctx, policy); err != nil {
		log := logr.FromContextOrDiscard(ctx)
		log.Error(err, "failed to update MaaSAuthPolicy status", "name", policy.Name)
	}
}

func (r *MaaSAuthPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch generated AuthPolicies so we re-reconcile when someone manually edits them.
	generatedAuthPolicy := &unstructured.Unstructured{}
	generatedAuthPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})

	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSAuthPolicy{}).
		// Watch HTTPRoutes so we re-reconcile when KServe creates/updates a route
		// (fixes race condition where MaaSAuthPolicy is created before HTTPRoute exists).
		Watches(&gatewayapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(
			r.mapHTTPRouteToMaaSAuthPolicies,
		)).
		// Watch MaaSModels so we re-reconcile when a model is created or deleted.
		Watches(&maasv1alpha1.MaaSModel{}, handler.EnqueueRequestsFromMapFunc(
			r.mapMaaSModelToMaaSAuthPolicies,
		)).
		// Watch generated AuthPolicies so manual edits get overwritten by the controller.
		Watches(generatedAuthPolicy, handler.EnqueueRequestsFromMapFunc(
			r.mapGeneratedAuthPolicyToParent,
		)).
		Complete(r)
}

// mapGeneratedAuthPolicyToParent maps a generated AuthPolicy back to its parent
// MaaSAuthPolicy using the labels set at creation time.
func (r *MaaSAuthPolicyReconciler) mapGeneratedAuthPolicyToParent(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "maas-controller" {
		return nil // not a generated policy
	}
	policyName := labels["maas.opendatahub.io/auth-policy"]
	policyNS := labels["maas.opendatahub.io/auth-policy-ns"]
	if policyName == "" || policyNS == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: policyName, Namespace: policyNS},
	}}
}

// mapMaaSModelToMaaSAuthPolicies returns reconcile requests for all MaaSAuthPolicies
// that reference the given MaaSModel.
func (r *MaaSAuthPolicyReconciler) mapMaaSModelToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
	model, ok := obj.(*maasv1alpha1.MaaSModel)
	if !ok {
		return nil
	}
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, p := range policies.Items {
		for _, ref := range p.Spec.ModelRefs {
			if ref == model.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// mapHTTPRouteToMaaSAuthPolicies returns reconcile requests for all MaaSAuthPolicies
// that reference models whose LLMInferenceService lives in the HTTPRoute's namespace.
func (r *MaaSAuthPolicyReconciler) mapHTTPRouteToMaaSAuthPolicies(ctx context.Context, obj client.Object) []reconcile.Request {
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
	// Find MaaSAuthPolicies that reference any of these models
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, p := range policies.Items {
		for _, ref := range p.Spec.ModelRefs {
			if modelNamesInNS[ref] {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: p.Name, Namespace: p.Namespace},
				})
				break
			}
		}
	}
	return requests
}
