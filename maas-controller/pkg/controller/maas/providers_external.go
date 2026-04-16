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

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// routeConditionProgrammed is the "Programmed" condition type for route parent status.
// gateway-api v1.2.1 only defines this as a Gateway condition (GatewayConditionProgrammed),
// but gateway controllers commonly set it on route parent status as well.
const routeConditionProgrammed = "Programmed"

// externalModelHandler implements BackendHandler for kind "ExternalModel".
type externalModelHandler struct {
	r *MaaSModelRefReconciler
}

// ReconcileRoute validates the HTTPRoute for an external model and populates status.
// The ExternalModel reconciler creates the "maas-model-<model.Name>" HTTPRoute in the
// model's namespace. This method validates that it exists and is accepted by the gateway.
func (h *externalModelHandler) ReconcileRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) error {
	// Fetch the referenced ExternalModel CR to get provider configuration
	externalModel := &maasv1alpha1.ExternalModel{}
	externalModelKey := types.NamespacedName{
		Name:      model.Spec.ModelRef.Name,
		Namespace: model.Namespace,
	}
	if err := h.r.Get(ctx, externalModelKey, externalModel); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ExternalModel %s not found in namespace %s", model.Spec.ModelRef.Name, model.Namespace)
		}
		return fmt.Errorf("failed to get ExternalModel %s: %w", model.Spec.ModelRef.Name, err)
	}

	routeName := model.Spec.ModelRef.Name
	routeNS := model.Namespace

	route := &gatewayapiv1.HTTPRoute{}
	key := client.ObjectKey{Name: routeName, Namespace: routeNS}
	if err := h.r.Get(ctx, key, route); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("HTTPRoute not found for ExternalModel, waiting for ExternalModel reconciler to create it",
				"routeName", routeName, "namespace", routeNS, "model", model.Name)
			// Clear stale route status so the model stays NotReady without requeue hot-looping
			model.Status.Endpoint = ""
			model.Status.HTTPRouteName = ""
			model.Status.HTTPRouteNamespace = ""
			model.Status.HTTPRouteGatewayName = ""
			model.Status.HTTPRouteGatewayNamespace = ""
			model.Status.HTTPRouteHostnames = nil
			return nil
		}
		return fmt.Errorf("failed to get HTTPRoute %s/%s: %w", routeNS, routeName, err)
	}

	expectedGatewayName := h.r.gatewayName()
	expectedGatewayNamespace := h.r.gatewayNamespace()
	gatewayFound := false
	gatewayAccepted := false
	var gatewayName string
	var gatewayNamespace string

	for _, parentRef := range route.Spec.ParentRefs {
		refName := string(parentRef.Name)
		refNS := routeNS
		if parentRef.Namespace != nil {
			refNS = string(*parentRef.Namespace)
		}
		if refName == expectedGatewayName && refNS == expectedGatewayNamespace {
			gatewayFound = true
			gatewayName = refName
			gatewayNamespace = refNS
			break
		}
		if gatewayName == "" {
			gatewayName = refName
			gatewayNamespace = refNS
		}
	}

	// Verify the gateway has accepted and programmed the route via status conditions
	if gatewayFound {
		for _, parent := range route.Status.Parents {
			pName := string(parent.ParentRef.Name)
			pNS := routeNS
			if parent.ParentRef.Namespace != nil {
				pNS = string(*parent.ParentRef.Namespace)
			}
			if pName == expectedGatewayName && pNS == expectedGatewayNamespace {
				for _, cond := range parent.Conditions {
					if cond.Type == string(gatewayapiv1.RouteConditionAccepted) && cond.Status == metav1.ConditionTrue {
						gatewayAccepted = true
					}
				}
				if gatewayAccepted {
					break
				}
			}
		}
	}

	var hostnames []string
	for _, hostname := range route.Spec.Hostnames {
		hostnames = append(hostnames, string(hostname))
	}

	if !gatewayFound {
		log.Error(nil, "HTTPRoute does not reference configured gateway",
			"routeName", routeName, "routeNamespace", routeNS,
			"expectedGateway", fmt.Sprintf("%s/%s", expectedGatewayNamespace, expectedGatewayName),
			"foundGateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName))
		return fmt.Errorf("HTTPRoute %s/%s does not reference gateway %s/%s (found: %s/%s)",
			routeNS, routeName, expectedGatewayNamespace, expectedGatewayName, gatewayNamespace, gatewayName)
	}

	if !gatewayAccepted {
		log.Info("HTTPRoute references correct gateway but not yet accepted and programmed",
			"routeName", routeName, "namespace", routeNS, "model", model.Name)
		model.Status.HTTPRouteName = routeName
		model.Status.HTTPRouteNamespace = routeNS
		// Don't set gateway/hostname fields until route is accepted
		return nil
	}

	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = routeNS
	model.Status.HTTPRouteGatewayName = gatewayName
	model.Status.HTTPRouteGatewayNamespace = gatewayNamespace
	model.Status.HTTPRouteHostnames = hostnames

	log.Info("HTTPRoute validated for ExternalModel",
		"routeName", routeName, "namespace", routeNS, "model", model.Name,
		"externalModel", externalModel.Name, "provider", externalModel.Spec.Provider,
		"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName), "hostnames", hostnames)

	return nil
}

// Status returns the model endpoint URL and whether the model is ready.
// ExternalModel is considered ready once the HTTPRoute is validated (no backend readiness probe).
func (h *externalModelHandler) Status(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (endpoint string, ready bool, err error) {
	if model.Status.HTTPRouteName == "" || model.Status.HTTPRouteGatewayName == "" {
		return "", false, nil
	}

	endpoint, err = h.GetModelEndpoint(ctx, log, model)
	if err != nil {
		return "", false, err
	}

	return endpoint, true, nil
}

// GetModelEndpoint returns the endpoint URL for the ExternalModel.
// Uses ExternalModel name (spec.modelRef.name) in the path to match the HTTPRoute
// created by the reconciler and BBR's model-provider-resolver store key.
func (h *externalModelHandler) GetModelEndpoint(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (string, error) {
	extModelName := model.Spec.ModelRef.Name
	if len(model.Status.HTTPRouteHostnames) > 0 {
		hostname := model.Status.HTTPRouteHostnames[0]
		return fmt.Sprintf("https://%s/%s/%s", hostname, model.Namespace, extModelName), nil
	}

	gatewayName := h.r.gatewayName()
	gatewayNS := h.r.gatewayNamespace()
	gateway := &gatewayapiv1.Gateway{}
	key := client.ObjectKey{Name: gatewayName, Namespace: gatewayNS}
	if err := h.r.Get(ctx, key, gateway); err != nil {
		return "", fmt.Errorf("failed to get gateway %s/%s: %w", gatewayNS, gatewayName, err)
	}

	for _, listener := range gateway.Spec.Listeners {
		if listener.Hostname != nil {
			return fmt.Sprintf("https://%s/%s/%s", string(*listener.Hostname), model.Namespace, extModelName), nil
		}
	}

	for _, addr := range gateway.Status.Addresses {
		if addr.Type != nil && *addr.Type == gatewayapiv1.HostnameAddressType {
			return fmt.Sprintf("https://%s/%s/%s", addr.Value, model.Namespace, extModelName), nil
		}
	}
	if len(gateway.Status.Addresses) > 0 {
		log.Info("Using IP-based gateway address; TLS hostname verification may fail",
			"address", gateway.Status.Addresses[0].Value, "model", extModelName)
		return fmt.Sprintf("https://%s/%s/%s", gateway.Status.Addresses[0].Value, model.Namespace, extModelName), nil
	}

	return "", fmt.Errorf("unable to determine endpoint: gateway %s/%s has no hostname or addresses", gatewayNS, gatewayName)
}

// CleanupOnDelete is called when the MaaSModelRef is deleted.
// ExternalModel: the ExternalModel reconciler handles cleanup of all resources via finalizer.
func (h *externalModelHandler) CleanupOnDelete(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) error {
	return nil
}

// externalModelRouteResolver returns the HTTPRoute name/namespace for ExternalModel.
// Used by findHTTPRouteForModel and by AuthPolicy/Subscription controllers to attach policies.
type externalModelRouteResolver struct{}

func (externalModelRouteResolver) HTTPRouteForModel(ctx context.Context, c client.Reader, model *maasv1alpha1.MaaSModelRef) (routeName, routeNamespace string, err error) {
	routeName = model.Spec.ModelRef.Name
	routeNamespace = model.Namespace
	return routeName, routeNamespace, nil
}
