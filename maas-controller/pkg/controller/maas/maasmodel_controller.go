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

// Default gateway name and namespace when not set via flags.
const (
	defaultGatewayName      = "maas-default-gateway"
	defaultGatewayNamespace = "openshift-ingress"
)

// MaaSModelReconciler reconciles a MaaSModel object
type MaaSModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// GatewayName and GatewayNamespace identify the Gateway used for model HTTPRoutes (configurable via flags).
	GatewayName      string
	GatewayNamespace string
}

func (r *MaaSModelReconciler) gatewayName() string {
	if r.GatewayName != "" {
		return r.GatewayName
	}
	return defaultGatewayName
}

func (r *MaaSModelReconciler) gatewayNamespace() string {
	if r.GatewayNamespace != "" {
		return r.GatewayNamespace
	}
	return defaultGatewayNamespace
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maasmodels/finalizers,verbs=update
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
//+kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=serving.kserve.io,resources=llminferenceservices,verbs=get;list;watch

const maasModelFinalizer = "maas.opendatahub.io/model-cleanup"

// Reconcile is part of the main kubernetes reconciliation loop
func (r *MaaSModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("MaaSModel", req.NamespacedName)

	model := &maasv1alpha1.MaaSModel{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch MaaSModel")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !model.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, log, model)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(model, maasModelFinalizer) {
		controllerutil.AddFinalizer(model, maasModelFinalizer)
		if err := r.Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
	}

	kind := model.Spec.ModelRef.Kind
	handler := GetBackendHandler(kind, r)
	if handler == nil {
		log.Error(nil, "unknown modelRef kind", "kind", kind)
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("unknown kind: %s", kind))
		return ctrl.Result{}, nil
	}

	if err := handler.ReconcileRoute(ctx, log, model); err != nil {
		if errors.Is(err, ErrKindNotImplemented) {
			r.updateStatusWithReason(ctx, model, "Failed", fmt.Sprintf("kind not implemented: %s", kind), "Unsupported")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to reconcile HTTPRoute")
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("Failed to reconcile HTTPRoute: %v", err))
		return ctrl.Result{}, err
	}

	endpoint, ready, err := handler.Status(ctx, log, model)
	if err != nil {
		if errors.Is(err, ErrKindNotImplemented) {
			model.Status.Endpoint = ""
			model.Status.Phase = "Failed"
			r.updateStatusWithReason(ctx, model, "Failed", fmt.Sprintf("kind not implemented: %s", kind), "Unsupported")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to update model status")
		model.Status.Endpoint = ""
		model.Status.Phase = "Failed"
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("Failed to update model status: %v", err))
		return ctrl.Result{}, err
	}
	model.Status.Endpoint = endpoint
	if ready {
		model.Status.Phase = "Ready"
		r.updateStatus(ctx, model, "Ready", "Successfully reconciled")
	} else {
		model.Status.Phase = "Pending"
		model.Status.Endpoint = ""
		r.updateStatus(ctx, model, "Pending", "Waiting for backend to become ready")
	}
	return ctrl.Result{}, nil
}

func (r *MaaSModelReconciler) handleDeletion(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(model, maasModelFinalizer) {
		// Clean up generated AuthPolicies for this model
		if err := r.deleteGeneratedPoliciesByLabel(ctx, log, model.Name, "AuthPolicy", "kuadrant.io", "v1"); err != nil {
			return ctrl.Result{}, err
		}

		// Clean up generated TokenRateLimitPolicies for this model
		if err := r.deleteGeneratedPoliciesByLabel(ctx, log, model.Name, "TokenRateLimitPolicy", "kuadrant.io", "v1alpha1"); err != nil {
			return ctrl.Result{}, err
		}

		// Kind-specific cleanup (e.g. delete HTTPRoute for ExternalModel; no-op for llmisvc)
		if handler := GetBackendHandler(model.Spec.ModelRef.Kind, r); handler != nil {
			if err := handler.CleanupOnDelete(ctx, log, model); err != nil {
				log.Error(err, "failed to clean up backend resources")
				return ctrl.Result{}, err
			}
		}

		// Remove finalizer so the MaaSModel can be deleted
		controllerutil.RemoveFinalizer(model, maasModelFinalizer)
		if err := r.Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// deleteGeneratedPoliciesByLabel finds and deletes all generated policies
// (AuthPolicy or TokenRateLimitPolicy) labeled with the given model name.
func (r *MaaSModelReconciler) deleteGeneratedPoliciesByLabel(ctx context.Context, log logr.Logger, modelName, kind, group, version string) error {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind + "List"})

	labelSelector := client.MatchingLabels{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
	}

	if err := r.List(ctx, policyList, labelSelector); err != nil {
		// If the CRD doesn't exist, skip (e.g. TokenRateLimitPolicy might not be installed)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to list %s resources for model %s: %w", kind, modelName, err)
	}

	for i := range policyList.Items {
		p := &policyList.Items[i]
		log.Info(fmt.Sprintf("Deleting generated %s on MaaSModel deletion", kind),
			"name", p.GetName(), "namespace", p.GetNamespace(), "model", modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete %s %s/%s: %w", kind, p.GetNamespace(), p.GetName(), err)
		}
	}

	return nil
}

func (r *MaaSModelReconciler) updateStatus(ctx context.Context, model *maasv1alpha1.MaaSModel, phase, message string) {
	r.updateStatusWithReason(ctx, model, phase, message, "")
}

// updateStatusWithReason sets Phase and Ready condition; when phase is "Failed", reason overrides the default "ReconcileFailed" (e.g. "Unsupported" for unimplemented kinds).
func (r *MaaSModelReconciler) updateStatusWithReason(ctx context.Context, model *maasv1alpha1.MaaSModel, phase, message, reason string) {
	model.Status.Phase = phase
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
		if reason != "" {
			condition.Reason = reason
		}
	}

	// Update condition
	found := false
	for i, c := range model.Status.Conditions {
		if c.Type == condition.Type {
			model.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		model.Status.Conditions = append(model.Status.Conditions, condition)
	}

	if err := r.Status().Update(ctx, model); err != nil {
		log := logr.FromContextOrDiscard(ctx)
		log.Error(err, "failed to update MaaSModel status", "name", model.Name)
		// Intentionally do not return the error so we do not re-queue on status update conflict/failure.
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaaSModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSModel{}).
		// Watch HTTPRoutes so we re-reconcile when KServe creates/updates a route
		// (fixes race condition where MaaSModel is created before HTTPRoute exists).
		Watches(&gatewayapiv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(
			r.mapHTTPRouteToMaaSModels,
		)).
		Complete(r)
}

// mapHTTPRouteToMaaSModels returns reconcile requests for all MaaSModels whose
// route namespace (ModelRef.Namespace or MaaSModel.Namespace) matches the HTTPRoute's namespace.
func (r *MaaSModelReconciler) mapHTTPRouteToMaaSModels(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayapiv1.HTTPRoute)
	if !ok {
		return nil
	}
	var models maasv1alpha1.MaaSModelList
	if err := r.List(ctx, &models); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, m := range models.Items {
		ns := m.Spec.ModelRef.Namespace
		if ns == "" {
			ns = m.Namespace
		}
		if ns == route.Namespace {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: m.Name, Namespace: m.Namespace},
			})
		}
	}
	return requests
}
