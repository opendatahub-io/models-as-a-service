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
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// llmisvcHandler implements BackendHandler for kind "llmisvc" (LLMInferenceService).
type llmisvcHandler struct {
	r *MaaSModelRefReconciler
}

func (h *llmisvcHandler) ReconcileRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) error {
	return h.validateLLMISvcHTTPRoute(ctx, log, model)
}

// validateLLMISvcHTTPRoute ensures an HTTPRoute exists for the referenced LLMInferenceService (by labels),
// populates MaaSModelRef status from the HTTPRoute and gateway ref.
func (h *llmisvcHandler) validateLLMISvcHTTPRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) error {
	routeNS := model.Namespace
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
		log.V(1).Info("HTTPRoute not found for LLMInferenceService, will retry when created", "llmisvcName", model.Spec.ModelRef.Name, "namespace", routeNS)
		return fmt.Errorf("%w: for LLMInferenceService %s in namespace %s", ErrHTTPRouteNotFound, model.Spec.ModelRef.Name, routeNS)
	}
	route := &routeList.Items[0]
	routeName := route.Name

	expectedGatewayName := h.r.gatewayName()
	expectedGatewayNamespace := h.r.gatewayNamespace()
	gatewayRef, err := h.resolveGatewayRef(ctx, log, model)
	if err != nil {
		return fmt.Errorf("resolve tenant gateway for model %s/%s: %w", model.Namespace, model.Name, err)
	}
	if gatewayRef.Name != "" {
		expectedGatewayName = gatewayRef.Name
		expectedGatewayNamespace = gatewayRef.Namespace
		log.V(4).Info("Using tenant gateway", "gateway", fmt.Sprintf("%s/%s", expectedGatewayNamespace, expectedGatewayName), "tenantRef", model.Spec.TenantRef)
	}

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

// resolveGatewayRef resolves the gateway reference for a MaaSModelRef.
// When spec.tenantRef is set, it looks up the named AITenant in the AITenant
// infrastructure namespace and uses its Status.GatewayRef directly.
// When spec.tenantRef is empty, it falls back to the existing namespace-based
// resolution via tenantGatewayRefForNamespace.
func (h *llmisvcHandler) resolveGatewayRef(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (maasv1alpha1.TenantGatewayRef, error) {
	if model.Spec.TenantRef != "" {
		aitenant := &maasv1alpha1.AITenant{}
		key := client.ObjectKey{
			Name:      model.Spec.TenantRef,
			Namespace: h.r.AITenantNamespace,
		}
		if err := h.r.Get(ctx, key, aitenant); err != nil {
			if apierrors.IsNotFound(err) {
				return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant %q not found in namespace %s", model.Spec.TenantRef, h.r.AITenantNamespace)
			}
			return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("failed to get AITenant %q: %w", model.Spec.TenantRef, err)
		}
		model.Status.ResolvedTenantRef = model.Spec.TenantRef
		ref := aitenant.Status.GatewayRef
		if ref.Name == "" || ref.Namespace == "" {
			return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant %q has no gateway reference in status", model.Spec.TenantRef)
		}
		log.V(4).Info("Resolved gateway from AITenant", "aiTenant", model.Spec.TenantRef, "gateway", fmt.Sprintf("%s/%s", ref.Namespace, ref.Name))
		return ref, nil
	}

	// Fall back to namespace-based resolution using the default tenant namespace.
	// Clear any previously resolved tenant (e.g. after spec.tenantRef is removed).
	model.Status.ResolvedTenantRef = ""
	return tenantGatewayRefForNamespace(
		ctx,
		h.r.Client,
		model.Namespace,
		h.r.DefaultTenantNamespace,
		h.r.gatewayName(),
		h.r.gatewayNamespace(),
		h.r.TenantNamespaceDiscoveryEnabled,
	)
}

func (h *llmisvcHandler) Status(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (endpoint string, ready bool, err error) {
	llmisvcNS := model.Namespace
	llmisvc := &kservev1alpha2.LLMInferenceService{}
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
	endpoint = h.getEndpointFromLLMISvc(llmisvc, model.Status.HTTPRouteHostnames)
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
func (h *llmisvcHandler) GetModelEndpoint(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (string, error) {
	if len(model.Status.HTTPRouteHostnames) > 0 {
		hostname := model.Status.HTTPRouteHostnames[0]
		return fmt.Sprintf("https://%s/%s", hostname, model.Name), nil
	}

	// Use the gateway from the model's status (populated by validateLLMISvcHTTPRoute)
	// which is tenant-aware. Fall back to default gateway if not set.
	gatewayName := model.Status.HTTPRouteGatewayName
	gatewayNS := model.Status.HTTPRouteGatewayNamespace
	if gatewayName == "" {
		gatewayName = h.r.gatewayName()
		gatewayNS = h.r.gatewayNamespace()
	}

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

const (
	addressNameGatewayExternal             = "gateway-external"
	addressNameGatewayExternalModelRouting = "gateway-external-model-routing"
)

// getEndpointFromLLMISvc returns the endpoint URL from LLMInferenceService status as-reported.
// Prefers model-routing (BBR) addresses (gateway-external-model-routing) over path-based ones
// (gateway-external), reflecting that BBR is the primary routing mode.
// When expectedHostnames is non-empty, only addresses whose hostname matches
// (case-insensitive per RFC 4343) are considered; this prevents selecting the wrong gateway
// when multiple gateways exist.
// When expectedHostnames is empty, preserves legacy behavior for single-gateway deployments.
// Returns "" when no suitable address is found; the caller (Status) falls through to
// GetModelEndpoint which derives the endpoint from Gateway/HTTPRoute metadata.
func (h *llmisvcHandler) getEndpointFromLLMISvc(llmisvc *kservev1alpha2.LLMInferenceService, expectedHostnames []string) string {
	hostSet := make(map[string]struct{}, len(expectedHostnames))
	for _, hn := range expectedHostnames {
		hostSet[strings.ToLower(hn)] = struct{}{}
	}
	filtering := len(hostSet) > 0

	// Prefer model-routing (BBR) addresses; fall back to path-based.
	for _, targetName := range []string{addressNameGatewayExternalModelRouting, addressNameGatewayExternal} {
		if u := h.selectAddress(llmisvc, targetName, hostSet, filtering); u != "" {
			return u
		}
	}

	// When filtering is active, don't fall back to unfiltered addresses — they may
	// belong to the wrong gateway.
	if filtering {
		return ""
	}
	// For unfiltered (legacy single-gateway) deployments, prefer base URLs (model-routing style)
	// over path-based URLs so status.endpoint is consistent with the BBR gateway entry-point.
	var fallbackURL string
	for _, addr := range llmisvc.Status.Addresses {
		if addr.URL == nil {
			continue
		}
		// Prefer base URLs (path "" or "/") — these are model-routing endpoints.
		if addr.URL.Path == "" || addr.URL.Path == "/" {
			return addr.URL.String()
		}
		if fallbackURL == "" {
			fallbackURL = addr.URL.String()
		}
	}
	// Check Status.URL as a base-URL candidate.
	if llmisvc.Status.URL != nil {
		if llmisvc.Status.URL.Path == "" || llmisvc.Status.URL.Path == "/" {
			return llmisvc.Status.URL.String()
		}
	}
	if fallbackURL != "" {
		return fallbackURL
	}
	if llmisvc.Status.URL != nil {
		return llmisvc.Status.URL.String()
	}
	return ""
}

func (h *llmisvcHandler) selectAddress(llmisvc *kservev1alpha2.LLMInferenceService, targetName string, hostSet map[string]struct{}, filtering bool) string {
	var urls []string
	for _, addr := range llmisvc.Status.Addresses {
		if addr.Name == nil || *addr.Name != targetName || addr.URL == nil {
			continue
		}
		if filtering {
			parsed := url.URL(*addr.URL)
			host := strings.ToLower(parsed.Hostname())
			if host == "" {
				continue
			}
			if _, ok := hostSet[host]; !ok {
				continue
			}
		}
		urls = append(urls, addr.URL.String())
	}
	for _, u := range urls {
		if strings.HasPrefix(u, "https://") {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// ResolveModelAlias returns the canonical BBR model ID for the referenced LLMInferenceService.
//
// It reads from LLMInferenceService.status.addresses[*].models[0].name, which KServe populates
// as the authoritative canonical ID in the format publishers/{namespace}/models/{model-name}.
// KServe is the source of truth for this value; MaaS reads and mirrors it.
//
// Return semantics:
//   - (alias, nil)  — alias resolved; caller should update status.resolvedModelAlias.
//   - ("", nil)     — LLMISVC found but addresses not yet populated; caller must preserve
//     the existing alias rather than clearing it.
//   - ("", err)     — transient API failure (e.g. API server unreachable); caller must
//     preserve the existing alias rather than clearing it.
func (h *llmisvcHandler) ResolveModelAlias(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (string, error) {
	llmisvc := &kservev1alpha2.LLMInferenceService{}
	key := client.ObjectKey{Name: model.Spec.ModelRef.Name, Namespace: model.Namespace}
	if err := h.r.Get(ctx, key, llmisvc); err != nil {
		return "", err
	}
	for _, addr := range llmisvc.Status.Addresses {
		if len(addr.Models) > 0 && addr.Models[0].Name != "" {
			return addr.Models[0].Name, nil
		}
	}
	return "", nil
}

func (h *llmisvcHandler) CleanupOnDelete(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) error {
	// llmisvc HTTPRoutes are owned by KServe; we do not delete them.
	return nil
}

// llmisvcRouteResolver resolves the HTTPRoute for a MaaSModelRef that references an LLMInferenceService.
type llmisvcRouteResolver struct{}

func (llmisvcRouteResolver) HTTPRouteForModel(ctx context.Context, c client.Reader, model *maasv1alpha1.MaaSModelRef) (routeName, routeNamespace string, err error) {
	llmisvcNS := model.Namespace
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
		return "", "", fmt.Errorf("%w: for LLMInferenceService %s in namespace %s", ErrHTTPRouteNotFound, model.Spec.ModelRef.Name, llmisvcNS)
	}
	route := &routeList.Items[0]
	return route.Name, route.Namespace, nil
}
