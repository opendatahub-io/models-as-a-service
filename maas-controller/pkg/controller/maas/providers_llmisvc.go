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
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// llmisvcHandler implements BackendHandler for kind "llmisvc" (LLMInferenceService).
type llmisvcHandler struct {
	r *MaaSModelReconciler
}

func (h *llmisvcHandler) ReconcileRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	return h.validateLLMISvcHTTPRoute(ctx, log, model)
}

// validateLLMISvcHTTPRoute ensures an HTTPRoute exists for the referenced LLMInferenceService (by labels),
// populates MaaSModel status from the HTTPRoute and gateway ref.
func (h *llmisvcHandler) validateLLMISvcHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	routeNS := model.Namespace
	if model.Spec.ModelRef.Namespace != "" {
		routeNS = model.Spec.ModelRef.Namespace
	}
	routeList := &gatewayapiv1.HTTPRouteList{}
	labelSelector := client.MatchingLabels{
		"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/part-of":   "llminferenceservice",
	}
	if err := h.r.List(ctx, routeList, client.InNamespace(routeNS), labelSelector); err != nil {
		return fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", model.Spec.ModelRef.Name, err)
	}
	if len(routeList.Items) == 0 {
		log.Error(nil, "HTTPRoute not found for LLMInferenceService", "llmisvcName", model.Spec.ModelRef.Name, "namespace", routeNS)
		return fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", model.Spec.ModelRef.Name, routeNS)
	}
	route := &routeList.Items[0]
	routeName := route.Name
	expectedGatewayName := h.r.gatewayName()
	expectedGatewayNamespace := h.r.gatewayNamespace()
	gatewayFound := false
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
	var hostnames []string
	for _, hostname := range route.Spec.Hostnames {
		hostnames = append(hostnames, string(hostname))
	}
	model.Status.HTTPRouteName = routeName
	model.Status.HTTPRouteNamespace = routeNS
	model.Status.HTTPRouteGatewayName = gatewayName
	model.Status.HTTPRouteGatewayNamespace = gatewayNamespace
	model.Status.HTTPRouteHostnames = hostnames
	if !gatewayFound {
		log.Error(nil, "HTTPRoute does not reference configured gateway",
			"routeName", routeName, "routeNamespace", routeNS,
			"expectedGateway", fmt.Sprintf("%s/%s", expectedGatewayNamespace, expectedGatewayName),
			"foundGateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName))
		return fmt.Errorf("HTTPRoute %s/%s does not reference gateway (expected: %s/%s, found: %s/%s). The LLMInferenceService must be configured to use %s/%s",
			routeNS, routeName, expectedGatewayNamespace, expectedGatewayName, gatewayNamespace, gatewayName, expectedGatewayNamespace, expectedGatewayName)
	}
	log.Info("HTTPRoute validated for LLMInferenceService",
		"routeName", routeName, "namespace", routeNS, "llmisvcName", model.Spec.ModelRef.Name,
		"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName), "hostnames", hostnames)
	return nil
}

func (h *llmisvcHandler) Status(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (endpoint string, ready bool, err error) {
	llmisvcNS := model.Namespace
	if model.Spec.ModelRef.Namespace != "" {
		llmisvcNS = model.Spec.ModelRef.Namespace
	}
	llmisvc := &kservev1alpha1.LLMInferenceService{}
	key := client.ObjectKey{Name: model.Spec.ModelRef.Name, Namespace: llmisvcNS}
	if err := h.r.Get(ctx, key, llmisvc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, fmt.Errorf("LLMInferenceService %s not found in namespace %s", model.Spec.ModelRef.Name, llmisvcNS)
		}
		return "", false, err
	}
	for _, c := range llmisvc.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
			break
		}
	}
	if !ready {
		return "", false, nil
	}
	endpoint = h.getEndpointFromLLMISvc(llmisvc)
	if endpoint == "" {
		endpoint, err = h.GetModelEndpoint(ctx, log, model)
		if err != nil {
			return "", false, err
		}
	}
	return endpoint, true, nil
}

// GetModelEndpoint returns the model endpoint URL using gateway/HTTPRoute hostname and path.
// Used when LLMInferenceService status does not expose an endpoint. ExternalModel and other kinds
// implement their own logic and need not use these path assumptions.
func (h *llmisvcHandler) GetModelEndpoint(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (string, error) {
	if len(model.Status.HTTPRouteHostnames) > 0 {
		hostname := model.Status.HTTPRouteHostnames[0]
		return fmt.Sprintf("https://%s/%s", hostname, model.Name), nil
	}
	gatewayName := h.r.gatewayName()
	gatewayNS := h.r.gatewayNamespace()
	gateway := &gatewayapiv1.Gateway{}
	key := client.ObjectKey{Name: gatewayName, Namespace: gatewayNS}
	if err := h.r.Get(ctx, key, gateway); err != nil {
		return "", fmt.Errorf("failed to get gateway %s/%s: %w", gatewayNS, gatewayName, err)
	}
	if len(gateway.Spec.Listeners) > 0 {
		for _, listener := range gateway.Spec.Listeners {
			if listener.Hostname != nil {
				return fmt.Sprintf("https://%s/%s", string(*listener.Hostname), model.Name), nil
			}
		}
	}
	if len(gateway.Status.Addresses) > 0 {
		for _, addr := range gateway.Status.Addresses {
			if addr.Type != nil && *addr.Type == gatewayapiv1.HostnameAddressType {
				return fmt.Sprintf("https://%s/%s", addr.Value, model.Name), nil
			}
		}
		return fmt.Sprintf("https://%s/%s", gateway.Status.Addresses[0].Value, model.Name), nil
	}
	return "", fmt.Errorf("unable to determine endpoint: gateway %s/%s has no hostname or addresses", gatewayNS, gatewayName)
}

// getEndpointFromLLMISvc returns the endpoint URL from LLMInferenceService status as-reported.
// Prefers gateway-external with https, then any gateway-external, then first address, then status.URL.
func (h *llmisvcHandler) getEndpointFromLLMISvc(llmisvc *kservev1alpha1.LLMInferenceService) string {
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

func (h *llmisvcHandler) CleanupOnDelete(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error {
	// llmisvc HTTPRoutes are owned by KServe; we do not delete them.
	return nil
}

// llmisvcRouteResolver resolves the HTTPRoute for a MaaSModel that references an LLMInferenceService.
type llmisvcRouteResolver struct{}

func (llmisvcRouteResolver) HTTPRouteForModel(ctx context.Context, c client.Reader, model *maasv1alpha1.MaaSModel) (routeName, routeNamespace string, err error) {
	llmisvcNS := model.Namespace
	if model.Spec.ModelRef.Namespace != "" {
		llmisvcNS = model.Spec.ModelRef.Namespace
	}
	routeList := &gatewayapiv1.HTTPRouteList{}
	labelSelector := client.MatchingLabels{
		"app.kubernetes.io/name":      model.Spec.ModelRef.Name,
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/part-of":   "llminferenceservice",
	}
	if err := c.List(ctx, routeList, client.InNamespace(llmisvcNS), labelSelector); err != nil {
		return "", "", fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", model.Spec.ModelRef.Name, err)
	}
	if len(routeList.Items) == 0 {
		return "", "", fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", model.Spec.ModelRef.Name, llmisvcNS)
	}
	route := &routeList.Items[0]
	return route.Name, route.Namespace, nil
}
