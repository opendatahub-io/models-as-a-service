package models

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/openai/openai-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/apis"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

// authResult represents the outcome of an authorization check attempt.
type authResult int

const (
	authGranted authResult = iota
	authDenied
	authRetry
)

type GatewayRef struct {
	Name      string
	Namespace string
}

// ListAvailableLLMs lists LLM models that the user has access to based on authorization checks.
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

	// Convert to models first, then filter based on authorization
	allModels := m.llmInferenceServicesToModels(instanceLLMs)

	// Filter models based on user authorization
	var authorizedModels []Model
	for _, model := range allModels {
		if m.userCanAccessModel(ctx, model, saToken) {
			authorizedModels = append(authorizedModels, model)
		}
	}

	return authorizedModels, nil
}

// userCanAccessModel checks if the user can access a specific model by making an authorization request.
// Uses the HTTP loopback approach to leverage the gateway's AuthPolicy as the single source of truth.
func (m *Manager) userCanAccessModel(ctx context.Context, model Model, saToken string) bool {
	if model.URL == nil {
		m.logger.Debug("Model URL is nil, denying access", "modelID", model.ID)
		return false
	}

	authCheckURL, err := url.JoinPath(model.URL.String(), m.authCheckEndpoint)
	if err != nil {
		m.logger.Debug("Failed to construct auth check URL", "modelID", model.ID, "error", err)
		return false
	}

	backoff := wait.Backoff{
		Steps:    4,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
	}

	var lastResult authResult
	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		lastResult = m.doAuthCheck(ctx, authCheckURL, saToken, model.ID)
		return lastResult != authRetry, nil
	}); err != nil {
		m.logger.Debug("Authorization check backoff failed", "modelID", model.ID, "error", err)
	}

	return lastResult == authGranted
}

// doAuthCheck performs a single authorization check attempt.
func (m *Manager) doAuthCheck(ctx context.Context, authCheckURL, saToken, modelID string) authResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, authCheckURL, nil)
	if err != nil {
		m.logger.Debug("Failed to create authorization request", "modelID", modelID, "error", err)
		return authRetry
	}

	req.Header.Set("Authorization", "Bearer "+saToken)

	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // To handle custom/self-signed certs we simply skip verification for now
			},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		m.logger.Debug("Authorization request failed", "modelID", modelID, "error", err)
		return authRetry
	}
	defer resp.Body.Close()

	m.logger.Debug("Authorization check response",
		"modelID", modelID,
		"statusCode", resp.StatusCode,
		"url", authCheckURL,
	)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return authGranted

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return authDenied

	case resp.StatusCode == http.StatusNotFound:
		// 404 is not a failure - endpoint might not exist but that's okay, allow access
		// TODO: Follow-up with fail-close implementation https://issues.redhat.com/browse/RHOAIENG-45883
		m.logger.Debug("Model endpoint returned 404, allowing access (not a denial)", "modelID", modelID)
		return authGranted

	case resp.StatusCode == http.StatusMethodNotAllowed:
		// 405 Method Not Allowed - this should not happen, something is misconfigured
		// Deny access as we cannot verify authorization
		m.logger.Debug("UNEXPECTED: Model endpoint returned 405 Method Not Allowed - this indicates a configuration issue. "+
			"HEAD requests should be supported by the gateway. Denying access as authorization cannot be verified",
			"modelID", modelID,
			"statusCode", resp.StatusCode,
			"url", authCheckURL,
		)
		return authDenied

	default:
		// Retry on server errors (5xx) or other unexpected codes
		m.logger.Debug("Unexpected status code, retrying",
			"modelID", modelID,
			"statusCode", resp.StatusCode,
		)
		return authRetry
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

func (m *Manager) llmInferenceServicesToModels(items []*kservev1alpha1.LLMInferenceService) []Model {
	models := make([]Model, 0, len(items))

	for _, item := range items {
		url := m.findLLMInferenceServiceURL(item)
		if url == nil {
			m.logger.Debug("Failed to find URL for LLMInferenceService",
				"namespace", item.Namespace,
				"name", item.Name,
			)
		}

		modelID := item.Name
		if item.Spec.Model.Name != nil && *item.Spec.Model.Name != "" {
			modelID = *item.Spec.Model.Name
		}

		models = append(models, Model{
			Model: openai.Model{
				ID:      modelID,
				Object:  "model",
				OwnedBy: item.Namespace,
				Created: item.CreationTimestamp.Unix(),
			},
			URL:     url,
			Ready:   m.checkLLMInferenceServiceReadiness(item),
			Details: m.extractModelDetails(item),
		})
	}

	return models
}

func (m *Manager) findLLMInferenceServiceURL(llmIsvc *kservev1alpha1.LLMInferenceService) *apis.URL {
	if llmIsvc.Status.URL != nil {
		return llmIsvc.Status.URL
	}

	if llmIsvc.Status.Address != nil && llmIsvc.Status.Address.URL != nil {
		return llmIsvc.Status.Address.URL
	}

	if len(llmIsvc.Status.Addresses) > 0 {
		return llmIsvc.Status.Addresses[0].URL
	}

	m.logger.Debug("No URL found for LLMInferenceService",
		"namespace", llmIsvc.Namespace,
		"name", llmIsvc.Name,
	)
	return nil
}

func (m *Manager) extractModelDetails(llmIsvc *kservev1alpha1.LLMInferenceService) *Details {
	annotations := llmIsvc.GetAnnotations()
	if annotations == nil {
		return nil
	}

	genaiUseCase := annotations[constant.AnnotationGenAIUseCase]
	description := annotations[constant.AnnotationDescription]
	displayName := annotations[constant.AnnotationDisplayName]

	// Only return Details if at least one field is populated
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
