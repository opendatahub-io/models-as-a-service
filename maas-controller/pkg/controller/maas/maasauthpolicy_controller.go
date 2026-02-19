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
	// Model-centric approach: for each model referenced by this auth policy,
	// find ALL auth policies for that model and build a single aggregated AuthPolicy.
	// Kuadrant only allows one AuthPolicy per HTTPRoute target.
	for _, modelName := range policy.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := r.findHTTPRouteForModel(ctx, log, policy.Namespace, modelName)
		if err != nil {
			if errors.Is(err, ErrModelNotFound) {
				log.Info("model not found, cleaning up generated AuthPolicy", "model", modelName)
				if delErr := r.deleteModelAuthPolicy(ctx, log, modelName); delErr != nil {
					return nil, fmt.Errorf("failed to clean up AuthPolicy for missing model %s: %w", modelName, delErr)
				}
				continue
			}
			return nil, fmt.Errorf("failed to resolve HTTPRoute for model %s: %w", modelName, err)
		}

		// Find ALL auth policies for this model (not just the current one)
		allPolicies, err := findAllAuthPoliciesForModel(ctx, r.Client, modelName)
		if err != nil {
			return nil, fmt.Errorf("failed to list auth policies for model %s: %w", modelName, err)
		}

		// Aggregate membership conditions from ALL auth policies
		var membershipConditions []interface{}
		var policyNames []string
		for _, ap := range allPolicies {
			policyNames = append(policyNames, ap.Name)
			for _, group := range ap.Spec.Subjects.Groups {
				if err := validateCELValue(group.Name, "group name"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s: %w", ap.Name, err)
				}
				membershipConditions = append(membershipConditions, map[string]interface{}{
					"operator": "incl", "selector": "auth.identity.user.groups", "value": group.Name,
				})
			}
			for _, user := range ap.Spec.Subjects.Users {
				if err := validateCELValue(user, "username"); err != nil {
					return nil, fmt.Errorf("invalid subject in MaaSAuthPolicy %s: %w", ap.Name, err)
				}
				membershipConditions = append(membershipConditions, map[string]interface{}{
					"operator": "eq", "selector": "auth.identity.user.username", "value": user,
				})
			}
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

		// Build aggregated authorization rule from ALL auth policies' subjects
		if len(membershipConditions) > 0 {
			var patterns []interface{}
			if len(membershipConditions) == 1 {
				patterns = membershipConditions
			} else {
				patterns = []interface{}{map[string]interface{}{"any": membershipConditions}}
			}
			rule["authorization"] = map[string]interface{}{
				"require-group-membership": map[string]interface{}{
					"metrics": false, "priority": int64(0),
					"patternMatching": map[string]interface{}{"patterns": patterns},
				},
			}
		}

		// Pass ALL user groups unfiltered in the response so TRLP predicates can
		// match against subscription groups (which may differ from auth policy groups).
		rule["response"] = map[string]interface{}{
			"success": map[string]interface{}{
				"filters": map[string]interface{}{
					"identity": map[string]interface{}{
						"json": map[string]interface{}{
							"properties": map[string]interface{}{
								"groups":     map[string]interface{}{"expression": "auth.identity.user.groups"},
								"groups_str": map[string]interface{}{"expression": `auth.identity.user.groups.join(",")`},
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

		// Build the aggregated AuthPolicy (one per model, covering all MaaSAuthPolicies)
		authPolicyName := fmt.Sprintf("maas-auth-%s", modelName)
		authPolicy := &unstructured.Unstructured{}
		authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
		authPolicy.SetName(authPolicyName)
		authPolicy.SetNamespace(httpRouteNS)
		authPolicy.SetLabels(map[string]string{
			"maas.opendatahub.io/model":    modelName,
			"app.kubernetes.io/managed-by": "maas-controller",
			"app.kubernetes.io/part-of":    "maas-auth-policy",
			"app.kubernetes.io/component":  "auth-policy",
		})
		authPolicy.SetAnnotations(map[string]string{
			"maas.opendatahub.io/auth-policies": strings.Join(policyNames, ","),
		})

		refs = append(refs, authPolicyRef{Name: authPolicyName, Namespace: httpRouteNS, Model: modelName})

		spec := map[string]interface{}{
			"targetRef": map[string]interface{}{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": httpRouteName},
			"rules":     rule,
		}
		if err := unstructured.SetNestedMap(authPolicy.Object, spec, "spec"); err != nil {
			return nil, fmt.Errorf("failed to set spec: %w", err)
		}

		// Create or update
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(authPolicy.GroupVersionKind())
		err = r.Get(ctx, client.ObjectKeyFromObject(authPolicy), existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, authPolicy); err != nil {
				return nil, fmt.Errorf("failed to create AuthPolicy for model %s: %w", modelName, err)
			}
			log.Info("AuthPolicy created", "name", authPolicyName, "model", modelName, "policies", policyNames)
		} else if err != nil {
			return nil, fmt.Errorf("failed to get existing AuthPolicy: %w", err)
		} else {
			if existing.GetAnnotations()["maas.opendatahub.io/managed"] == "false" {
				log.Info("AuthPolicy opted out, skipping", "name", authPolicyName)
			} else {
				mergedAnnotations := existing.GetAnnotations()
				if mergedAnnotations == nil {
					mergedAnnotations = make(map[string]string)
				}
				for k, v := range authPolicy.GetAnnotations() {
					mergedAnnotations[k] = v
				}
				existing.SetAnnotations(mergedAnnotations)
				existing.SetLabels(authPolicy.GetLabels())
				if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
					return nil, fmt.Errorf("failed to update spec: %w", err)
				}
				if err := r.Update(ctx, existing); err != nil {
					return nil, fmt.Errorf("failed to update AuthPolicy for model %s: %w", modelName, err)
				}
				log.Info("AuthPolicy updated", "name", authPolicyName, "model", modelName, "policies", policyNames)
			}
		}
	}
	return refs, nil
}

// findHTTPRouteForModel delegates to the shared helper in helpers.go.
func (r *MaaSAuthPolicyReconciler) findHTTPRouteForModel(ctx context.Context, log logr.Logger, defaultNS, modelName string) (string, string, error) {
	return findHTTPRouteForModel(ctx, r.Client, defaultNS, modelName)
}

// deleteModelAuthPolicy deletes the aggregated AuthPolicy for a model by label.
func (r *MaaSAuthPolicyReconciler) deleteModelAuthPolicy(ctx context.Context, log logr.Logger, modelName string) error {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"})
	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-auth-policy",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		return fmt.Errorf("failed to list AuthPolicies for cleanup: %w", err)
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		log.Info("Deleting AuthPolicy", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete AuthPolicy %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}

func (r *MaaSAuthPolicyReconciler) handleDeletion(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(policy, maasAuthPolicyFinalizer) {
		for _, modelName := range policy.Spec.ModelRefs {
			log.Info("Deleting model AuthPolicy so remaining policies can rebuild it", "model", modelName)
			if err := r.deleteModelAuthPolicy(ctx, log, modelName); err != nil {
				log.Error(err, "failed to clean up AuthPolicy, will retry", "model", modelName)
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

// mapGeneratedAuthPolicyToParent maps a generated AuthPolicy back to any
// MaaSAuthPolicy that references the same model. The AuthPolicy is per-model
// (aggregated), so we use the model label to find a policy to trigger reconciliation.
func (r *MaaSAuthPolicyReconciler) mapGeneratedAuthPolicyToParent(ctx context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "maas-controller" {
		return nil
	}
	modelName := labels["maas.opendatahub.io/model"]
	if modelName == "" {
		return nil
	}
	ap := findAnyAuthPolicyForModel(ctx, r.Client, modelName)
	if ap == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: ap.Name, Namespace: ap.Namespace},
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
