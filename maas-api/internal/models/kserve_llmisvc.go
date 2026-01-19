package models

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/openai/openai-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

type authResult int

const (
	authGranted authResult = iota
	authDenied
	authRetry
)

const (
	authCheckTimeout  = 3 * time.Second
	authCheckEndpoint = "/v1/chat/completions"
)

type GatewayRef struct {
	Name      string
	Namespace string
}

// accessibleLLM pairs an LLMInferenceService with its accessible URL.
type accessibleLLM struct {
	llmIsvc *kservev1alpha1.LLMInferenceService
	url     *apis.URL
}

// ListAvailableLLMs lists LLM models that the user has access to.
// Addresses are checked in priority order: external gateways first, then internal.
// The returned model URL is the address that was actually accessible.
func (m *Manager) ListAvailableLLMs(ctx context.Context, saToken string) ([]Model, error) {
	list, err := m.llmIsvcLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list LLMInferenceServices: %w", err)
	}

	var instanceLLMs []*kservev1alpha1.LLMInferenceService
	for _, llmIsvc := range list {
		if m.partOfMaaSInstance(llmIsvc) {
			instanceLLMs = append(instanceLLMs, llmIsvc)
		}
	}

	var authorized []accessibleLLM
	for _, llmIsvc := range instanceLLMs {
		if addr := m.getAccessibleAddress(ctx, llmIsvc, saToken); addr != nil {
			authorized = append(authorized, accessibleLLM{llmIsvc: llmIsvc, url: addr.URL})
		}
	}

	return m.toModels(authorized), nil
}

func (m *Manager) extractModelID(llmIsvc *kservev1alpha1.LLMInferenceService) string {
	if llmIsvc.Spec.Model.Name != nil && *llmIsvc.Spec.Model.Name != "" {
		return *llmIsvc.Spec.Model.Name
	}
	return llmIsvc.Name
}

// getAccessibleAddress returns the first accessible address for the LLMInferenceService, or nil if none.
func (m *Manager) getAccessibleAddress(ctx context.Context, llmIsvc *kservev1alpha1.LLMInferenceService, saToken string) *duckv1.Addressable {
	addresses := m.getPrioritizedAddresses(llmIsvc)
	if len(addresses) == 0 {
		m.logger.Debug("No addresses available for LLMInferenceService",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
		)
		return nil
	}

	modelID := m.extractModelID(llmIsvc)

	backoff := wait.Backoff{
		Steps:    4,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
	}

	var accessibleAddr *duckv1.Addressable
	var lastResult authResult
	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		lastResult, accessibleAddr = m.tryAuthWithAddresses(ctx, addresses, saToken, modelID)
		return lastResult != authRetry, nil
	}); err != nil {
		m.logger.Debug("Authorization check backoff failed",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
			"error", err,
		)
		return nil
	}

	return accessibleAddr
}

// tryAuthWithAddresses tries all addresses and returns granted if ANY succeeds.
// This is important for multi-tenant scenarios where different gateways have different AuthPolicies -
// a token denied by one gateway might be authorized by another.
func (m *Manager) tryAuthWithAddresses(ctx context.Context, addresses []duckv1.Addressable, saToken, modelID string) (authResult, *duckv1.Addressable) {
	var hasRetryableFailure bool

	for i := range addresses {
		addr := &addresses[i]
		if addr.URL == nil {
			continue
		}

		endpoint, err := url.JoinPath(addr.URL.String(), authCheckEndpoint)
		if err != nil {
			m.logger.Debug("Failed to create endpoint",
				"modelID", modelID,
				"url", addr.URL.String(),
				"error", err,
			)
			continue
		}

		switch m.doAuthCheck(ctx, endpoint, saToken, modelID, addr.Name) {
		case authGranted:
			return authGranted, addr
		case authRetry:
			hasRetryableFailure = true
		case authDenied:
			m.logger.Debug("Address denied, trying next",
				"modelID", modelID,
				"address", addr.URL.String(),
			)
			continue
		}
	}

	if hasRetryableFailure {
		return authRetry, nil
	}
	return authDenied, nil
}

func (m *Manager) doAuthCheck(ctx context.Context, authCheckURL, saToken, modelID string, addressName *string) authResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, authCheckURL, nil)
	if err != nil {
		m.logger.Debug("Failed to create authorization request",
			"modelID", modelID,
			"error", err,
		)
		return authRetry
	}

	req.Header.Set("Authorization", "Bearer "+saToken)

	client := &http.Client{
		Timeout: authCheckTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // To handle custom/self-signed certs we simply skip verification for now
			},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		m.logger.Debug("Authorization request failed",
			"modelID", modelID,
			"url", authCheckURL,
			"addressName", ptr.Deref(addressName, ""),
			"error", err,
		)
		return authRetry
	}
	defer resp.Body.Close()

	m.logger.Debug("Authorization check response",
		"modelID", modelID,
		"statusCode", resp.StatusCode,
		"url", authCheckURL,
		"addressName", ptr.Deref(addressName, ""),
	)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return authGranted

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return authDenied

	case resp.StatusCode == http.StatusNotFound:
		// 404 means we cannot verify authorization - deny access (fail-closed)
		// See: https://issues.redhat.com/browse/RHOAIENG-45883
		m.logger.Debug("Model endpoint returned 404, denying access (cannot verify authorization)",
			"modelID", modelID,
			"addressName", ptr.Deref(addressName, ""),
		)
		return authDenied

	case resp.StatusCode == http.StatusMethodNotAllowed:
		// 405 Method Not Allowed - this should not happen, something is misconfigured
		// Deny access as we cannot verify authorization
		m.logger.Debug("UNEXPECTED: Model endpoint returned 405 Method Not Allowed - this indicates a configuration issue. "+
			"OPTIONS requests should be supported by the gateway. Denying access as authorization cannot be verified",
			"modelID", modelID,
			"statusCode", resp.StatusCode,
			"url", authCheckURL,
			"addressName", ptr.Deref(addressName, ""),
		)
		return authDenied

	default:
		m.logger.Debug("Unexpected status code, retrying",
			"modelID", modelID,
			"statusCode", resp.StatusCode,
			"addressName", ptr.Deref(addressName, ""),
		)
		return authRetry
	}
}

// getPrioritizedAddresses returns addresses sorted by priority: external > internal > others.
// status.URL is always appended last as a best-effort fallback.
func (m *Manager) getPrioritizedAddresses(llmIsvc *kservev1alpha1.LLMInferenceService) []duckv1.Addressable {
	var addresses []duckv1.Addressable

	addresses = append(addresses, llmIsvc.Status.Addresses...)

	if len(addresses) == 0 && llmIsvc.Status.Address != nil && llmIsvc.Status.Address.URL != nil {
		addresses = append(addresses, *llmIsvc.Status.Address)
	}

	slices.SortStableFunc(addresses, func(a, b duckv1.Addressable) int {
		return addressPriority(a) - addressPriority(b)
	})

	if llmIsvc.Status.URL != nil {
		addresses = append(addresses, duckv1.Addressable{URL: llmIsvc.Status.URL})
	}

	return addresses
}

func addressPriority(addr duckv1.Addressable) int {
	name := ""
	if addr.Name != nil {
		name = strings.ToLower(*addr.Name)
	}

	switch {
	case strings.Contains(name, "external"):
		return 0
	case strings.Contains(name, "internal"):
		return 1
	default:
		return 2
	}
}

// partOfMaaSInstance checks if the given LLMInferenceService is part of this "MaaS instance". This means that it is
// either directly referenced by the gateway that has MaaS capabilities, or it is referenced by an HTTPRoute that is managed by the gateway.
// The gateway is part of the component configuration.
func (m *Manager) partOfMaaSInstance(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router == nil {
		return false
	}

	return m.hasDirectGatewayReference(llmIsvc) ||
		m.hasHTTPRouteSpecRefToGateway(llmIsvc) ||
		m.hasReferencedRouteAttachedToGateway(llmIsvc) ||
		m.hasManagedRouteAttachedToGateway(llmIsvc)
}

func (m *Manager) toModels(items []accessibleLLM) []Model {
	models := make([]Model, 0, len(items))

	for _, item := range items {
		models = append(models, Model{
			Model: openai.Model{
				ID:      m.extractModelID(item.llmIsvc),
				Object:  "model",
				OwnedBy: item.llmIsvc.Namespace,
				Created: item.llmIsvc.CreationTimestamp.Unix(),
			},
			URL:     item.url,
			Ready:   m.checkLLMInferenceServiceReadiness(item.llmIsvc),
			Details: m.extractModelDetails(item.llmIsvc),
		})
	}

	return models
}

func (m *Manager) extractModelDetails(llmIsvc *kservev1alpha1.LLMInferenceService) *Details {
	annotations := llmIsvc.GetAnnotations()
	if annotations == nil {
		return nil
	}

	genaiUseCase := annotations[constant.AnnotationGenAIUseCase]
	description := annotations[constant.AnnotationDescription]
	displayName := annotations[constant.AnnotationDisplayName]

	if genaiUseCase == "" && description == "" && displayName == "" {
		return nil
	}

	return &Details{
		GenAIUseCase: genaiUseCase,
		Description:  description,
		DisplayName:  displayName,
	}
}

func (m *Manager) checkLLMInferenceServiceReadiness(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.DeletionTimestamp != nil {
		return false
	}

	if llmIsvc.Generation > 0 && llmIsvc.Status.ObservedGeneration != llmIsvc.Generation {
		m.logger.Debug("ObservedGeneration is stale, not ready yet",
			"observed_generation", llmIsvc.Status.ObservedGeneration,
			"expected_generation", llmIsvc.Generation,
		)
		return false
	}

	if len(llmIsvc.Status.Conditions) == 0 {
		m.logger.Debug("No conditions found for LLMInferenceService",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
		)
		return false
	}

	for _, cond := range llmIsvc.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			return false
		}
	}

	return true
}

func (m *Manager) hasDirectGatewayReference(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Gateway == nil {
		return false
	}

	for _, ref := range llmIsvc.Spec.Router.Gateway.Refs {
		if string(ref.Name) != m.gatewayRef.Name {
			continue
		}

		refNamespace := llmIsvc.Namespace
		if ref.Namespace != "" {
			refNamespace = string(ref.Namespace)
		}

		if refNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}

func (m *Manager) hasHTTPRouteSpecRefToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil || llmIsvc.Spec.Router.Route.HTTP.Spec == nil {
		return false
	}

	for _, parentRef := range llmIsvc.Spec.Router.Route.HTTP.Spec.ParentRefs {
		if string(parentRef.Name) != m.gatewayRef.Name {
			continue
		}

		parentNamespace := llmIsvc.Namespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}

		if parentNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}

func (m *Manager) hasReferencedRouteAttachedToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil || len(llmIsvc.Spec.Router.Route.HTTP.Refs) == 0 {
		return false
	}

	for _, routeRef := range llmIsvc.Spec.Router.Route.HTTP.Refs {
		route, err := m.httpRouteLister.HTTPRoutes(llmIsvc.Namespace).Get(routeRef.Name)
		if err != nil {
			m.logger.Debug("HTTPRoute not in cache",
				"namespace", llmIsvc.Namespace,
				"name", routeRef.Name,
				"error", err,
			)
			continue
		}
		if route == nil {
			continue
		}

		if m.routeAttachedToGateway(route, llmIsvc.Namespace) {
			return true
		}
	}

	return false
}

func (m *Manager) hasManagedRouteAttachedToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil {
		return false
	}

	httpRoute := llmIsvc.Spec.Router.Route.HTTP
	if httpRoute.Spec != nil || len(httpRoute.Refs) > 0 {
		return false
	}

	selector := labels.SelectorFromSet(labels.Set{
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/name":      llmIsvc.Name,
		"app.kubernetes.io/part-of":   "llminferenceservice",
	})

	routes, err := m.httpRouteLister.HTTPRoutes(llmIsvc.Namespace).List(selector)
	if err != nil {
		m.logger.Debug("Failed to list HTTPRoutes for LLM",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
			"error", err,
		)
		return false
	}

	for _, route := range routes {
		if m.routeAttachedToGateway(route, llmIsvc.Namespace) {
			return true
		}
	}

	return false
}

func (m *Manager) routeAttachedToGateway(route *gwapiv1.HTTPRoute, defaultNamespace string) bool {
	for _, parentRef := range route.Spec.ParentRefs {
		if string(parentRef.Name) != m.gatewayRef.Name {
			continue
		}

		parentNamespace := defaultNamespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}

		if parentNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}
