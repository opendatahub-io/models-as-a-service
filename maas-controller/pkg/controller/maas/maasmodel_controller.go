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
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
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

// MaaSModelReconciler reconciles a MaaSModel object
type MaaSModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	// Only reconcile HTTPRoute for known kinds. Unknown kind -> set status to Failed and exit.
	kind := model.Spec.ModelRef.Kind
	if kind != "llmisvc" && kind != "ExternalModel" {
		log.Error(nil, "unknown modelRef kind", "kind", kind)
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("unknown kind: %s", kind))
		return ctrl.Result{}, nil
	}

	// Reconcile HTTPRoute: for ExternalModel create/update route; for llmisvc validate route exists (populates MaaSModel status from HTTPRoute/Gateway only).
	if err := r.reconcileHTTPRoute(ctx, log, model); err != nil {
		log.Error(err, "failed to reconcile HTTPRoute")
		r.updateStatus(ctx, model, "Failed", fmt.Sprintf("Failed to reconcile HTTPRoute: %v", err))
		return ctrl.Result{}, err
	}

	// Auth for model routes is managed by MaaSAuthPolicy only (one AuthPolicy per route).

	// Update status based on referenced model
	modelStatusFailed := false
	if err := r.updateModelStatus(ctx, log, model); err != nil {
		log.Error(err, "failed to update model status")
		modelStatusFailed = true
	}

	// Set Ready unless updateModelStatus failed in the current reconciliation
	if !modelStatusFailed {
		r.updateStatus(ctx, model, "Ready", "Successfully reconciled")
	}
	return ctrl.Result{}, nil
}

func (r *MaaSModelReconciler) reconcileHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// Only ExternalModel gets an HTTPRoute created by this controller.
	if model.Spec.ModelRef.Kind == "ExternalModel" {
		return r.createOrUpdateHTTPRoute(ctx, log, model)
	}
	// llmisvc: validate HTTPRoute exists (created by LLMInferenceService controller) and populate MaaSModel status from it.
	if model.Spec.ModelRef.Kind == "llmisvc" {
		return r.validateLLMISvcHTTPRoute(ctx, log, model)
	}
	return fmt.Errorf("unknown kind: %s", model.Spec.ModelRef.Kind)
}

// validateLLMISvcHTTPRoute ensures an HTTPRoute exists for the referenced LLMInferenceService (by labels),
// populates MaaSModel status from the HTTPRoute and gateway ref.
func (r *MaaSModelReconciler) validateLLMISvcHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	routeNS := model.Namespace
	if model.Spec.ModelRef.Namespace != "" {
		routeNS = model.Spec.ModelRef.Namespace
	}

	// HTTPRoutes for LLMInferenceService have these labels:
	// app.kubernetes.io/name: <llmisvc-name>, component: llminferenceservice-router, part-of: llminferenceservice
	routeList := &gatewayapiv1.HTTPRouteList{}
	labelSelector := client.MatchingLabels{
		"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/part-of":   "llminferenceservice",
	}

	if err := r.List(ctx, routeList, client.InNamespace(routeNS), labelSelector); err != nil {
		return fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", model.Spec.ModelRef.Name, err)
	}

	if len(routeList.Items) == 0 {
		log.Error(nil, "HTTPRoute not found for LLMInferenceService", "llmisvcName", model.Spec.ModelRef.Name, "namespace", routeNS)
		return fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", model.Spec.ModelRef.Name, routeNS)
	}

	// Use the first matching HTTPRoute
	route := &routeList.Items[0]
	routeName := route.Name

	// Validate that the HTTPRoute references maas-default-gateway
	const expectedGatewayName = "maas-default-gateway"
	const expectedGatewayNamespace = "openshift-ingress"

	gatewayFound := false
	var gatewayName string
	var gatewayNamespace string

	for _, parentRef := range route.Spec.ParentRefs {
		refName := string(parentRef.Name)
		refNS := routeNS // Default to HTTPRoute namespace
		if parentRef.Namespace != nil {
			refNS = string(*parentRef.Namespace)
		}

		// Check if this parent reference points to maas-default-gateway
		if refName == expectedGatewayName && refNS == expectedGatewayNamespace {
			gatewayFound = true
			gatewayName = refName
			gatewayNamespace = refNS
			break
		}

		// Track the first gateway reference we find (for status reporting)
		if gatewayName == "" {
			gatewayName = refName
			gatewayNamespace = refNS
		}
	}

	// Extract hostnames from HTTPRoute
	var hostnames []string
	for _, hostname := range route.Spec.Hostnames {
		hostnames = append(hostnames, string(hostname))
	}

	// Update status with HTTPRoute information
	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = routeNS
	model.Status.HTTPRouteGatewayName = gatewayName
	model.Status.HTTPRouteGatewayNamespace = gatewayNamespace
	model.Status.HTTPRouteHostnames = hostnames

	// Validate gateway reference
	if !gatewayFound {
		log.Error(nil, "HTTPRoute does not reference maas-default-gateway",
			"routeName", routeName,
			"routeNamespace", routeNS,
			"expectedGateway", fmt.Sprintf("%s/%s", expectedGatewayNamespace, expectedGatewayName),
			"foundGateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName))
		return fmt.Errorf("HTTPRoute %s/%s does not reference maas-default-gateway (expected: %s/%s, found: %s/%s). The LLMInferenceService must be configured to use maas-default-gateway",
			routeNS, routeName, expectedGatewayNamespace, expectedGatewayName, gatewayNamespace, gatewayName)
	}

	log.Info("HTTPRoute validated for LLMInferenceService",
		"routeName", routeName,
		"namespace", routeNS,
		"llmisvcName", model.Spec.ModelRef.Name,
		"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName),
		"hostnames", hostnames)

	return nil
}

func (r *MaaSModelReconciler) createOrUpdateHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	routeName := fmt.Sprintf("maas-model-%s", model.Name)
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: model.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		// Set owner reference
		if err := controllerutil.SetControllerReference(model, route, r.Scheme); err != nil {
			return err
		}

		// Determine namespace for backend service
		backendNS := model.Namespace
		if model.Spec.ModelRef.Namespace != "" {
			backendNS = model.Spec.ModelRef.Namespace
		}

		// Build HTTPRoute spec
		// This routes requests to the model endpoint
		route.Spec = gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{
						Name:      gatewayapiv1.ObjectName("maas-default-gateway"),
						Namespace: (*gatewayapiv1.Namespace)(&model.Namespace),
					},
				},
			},
			Hostnames: []gatewayapiv1.Hostname{
				"maas.*", // Match any hostname under maas domain
			},
			Rules: []gatewayapiv1.HTTPRouteRule{
				{
					Matches: []gatewayapiv1.HTTPRouteMatch{
						{
							Path: &gatewayapiv1.HTTPPathMatch{
								Type:  ptr(gatewayapiv1.PathMatchPathPrefix),
								Value: ptr(fmt.Sprintf("/%s", model.Name)),
							},
						},
					},
					BackendRefs: []gatewayapiv1.HTTPBackendRef{
						{
							BackendRef: gatewayapiv1.BackendRef{
								BackendObjectReference: gatewayapiv1.BackendObjectReference{
									Group: ptr(gatewayapiv1.Group("")),
									Kind:  ptr(gatewayapiv1.Kind("Service")),
									Name:  gatewayapiv1.ObjectName(model.Spec.ModelRef.Name),
									Namespace: func() *gatewayapiv1.Namespace {
										ns := gatewayapiv1.Namespace(backendNS)
										return &ns
									}(),
									Port: ptr(gatewayapiv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to create/update HTTPRoute: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("HTTPRoute reconciled", "operation", op, "name", routeName)
	}

	// Update status with HTTPRoute information
	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = model.Namespace

	return nil
}

func (r *MaaSModelReconciler) updateModelStatus(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// For llmisvc: read endpoint and readiness directly from LLMInferenceService status.
	if model.Spec.ModelRef.Kind == "llmisvc" {
		llmisvcNS := model.Namespace
		if model.Spec.ModelRef.Namespace != "" {
			llmisvcNS = model.Spec.ModelRef.Namespace
		}
		llmisvc := &kservev1alpha1.LLMInferenceService{}
		key := client.ObjectKey{Name: model.Spec.ModelRef.Name, Namespace: llmisvcNS}
		if err := r.Get(ctx, key, llmisvc); err != nil {
			if apierrors.IsNotFound(err) {
				model.Status.Phase = "Failed"
				model.Status.Endpoint = ""
				return nil
			}
			return err
		}
		ready := false
		for _, c := range llmisvc.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			model.Status.Phase = "Pending"
			model.Status.Endpoint = ""
			return nil
		}
		endpoint := r.getEndpointFromLLMISvc(llmisvc)
		if endpoint == "" {
			var err error
			endpoint, err = r.getModelEndpoint(ctx, log, model)
			if err != nil {
				log.Error(err, "failed to get model endpoint fallback")
				model.Status.Phase = "Failed"
				model.Status.Endpoint = ""
				return nil
			}
		}
		model.Status.Endpoint = endpoint
		model.Status.Phase = "Ready"
		return nil
	}

	if model.Spec.ModelRef.Kind == "ExternalModel" {
		endpoint, err := r.getModelEndpoint(ctx, log, model)
		if err != nil {
			log.Error(err, "failed to get model endpoint for ExternalModel")
			model.Status.Phase = "Failed"
			model.Status.Endpoint = ""
			return nil
		}
		model.Status.Endpoint = endpoint
		model.Status.Phase = "Ready"
	}

	return nil
}

// getEndpointFromLLMISvc returns the endpoint URL from LLMInferenceService status as-reported.
// Prefers gateway-external with https, then any gateway-external, then first address, then status.URL.
func (r *MaaSModelReconciler) getEndpointFromLLMISvc(llmisvc *kservev1alpha1.LLMInferenceService) string {
	var gatewayExternalURLs []string
	for _, addr := range llmisvc.Status.Addresses {
		if addr.Name != nil && *addr.Name == "gateway-external" && addr.URL != nil {
			gatewayExternalURLs = append(gatewayExternalURLs, addr.URL.String())
		}
	}
	for _, u := range gatewayExternalURLs {
		if strings.HasPrefix(u, "https://") {
			return u
		}
	}
	if len(gatewayExternalURLs) > 0 {
		return gatewayExternalURLs[0]
	}
	if len(llmisvc.Status.Addresses) > 0 && llmisvc.Status.Addresses[0].URL != nil {
		return llmisvc.Status.Addresses[0].URL.String()
	}
	if llmisvc.Status.URL != nil {
		return llmisvc.Status.URL.String()
	}
	return ""
}

// getModelEndpoint constructs the endpoint URL from MaaSModel status (HTTPRoute hostnames) or Gateway.
func (r *MaaSModelReconciler) getModelEndpoint(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (string, error) {
	// First, try to use HTTPRoute hostname from status (if already set)
	if len(model.Status.HTTPRouteHostnames) > 0 {
		hostname := model.Status.HTTPRouteHostnames[0]
		return fmt.Sprintf("https://%s/%s", hostname, model.Name), nil
	}

	// If HTTPRoute hostname not available, get it from the gateway
	// Get the gateway to find its external address/hostname
	gatewayName := "maas-default-gateway"
	gatewayNS := "openshift-ingress"

	gateway := &gatewayapiv1.Gateway{}
	key := client.ObjectKey{
		Name:      gatewayName,
		Namespace: gatewayNS,
	}

	if err := r.Get(ctx, key, gateway); err != nil {
		return "", fmt.Errorf("failed to get gateway %s/%s: %w", gatewayNS, gatewayName, err)
	}

	// Try to get hostname from gateway listeners first
	if len(gateway.Spec.Listeners) > 0 {
		for _, listener := range gateway.Spec.Listeners {
			if listener.Hostname != nil {
				hostname := string(*listener.Hostname)
				return fmt.Sprintf("https://%s/%s", hostname, model.Name), nil
			}
		}
	}

	// Fall back to gateway status addresses (external addresses)
	if len(gateway.Status.Addresses) > 0 {
		// Prefer hostname type addresses
		for _, addr := range gateway.Status.Addresses {
			if addr.Type != nil && *addr.Type == gatewayapiv1.HostnameAddressType {
				return fmt.Sprintf("https://%s/%s", addr.Value, model.Name), nil
			}
		}
		// If no hostname type, use the first address
		return fmt.Sprintf("https://%s/%s", gateway.Status.Addresses[0].Value, model.Name), nil
	}

	return "", fmt.Errorf("unable to determine endpoint: gateway %s/%s has no hostname or addresses", gatewayNS, gatewayName)
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

		// Only clean up HTTPRoute for ExternalModel (llmisvc HTTPRoutes are managed by LLMInferenceService controller)
		if model.Spec.ModelRef.Kind != "llmisvc" {
			routeName := fmt.Sprintf("maas-model-%s", model.Name)
			route := &gatewayapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      routeName,
					Namespace: model.Namespace,
				},
			}
			if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete HTTPRoute")
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
	}
}

// Helper function to get pointer to value
func ptr[T any](v T) *T {
	return &v
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

// mapHTTPRouteToMaaSModels returns reconcile requests for all MaaSModels that
// reference an LLMInferenceService in the same namespace as the HTTPRoute.
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
