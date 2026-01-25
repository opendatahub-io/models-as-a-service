package models

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
// userContext is optional but required when using Keycloak to determine tier for admin SA token.
func (m *Manager) ListAvailableLLMs(ctx context.Context, saToken string, userContext interface{}) ([]Model, error) {
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
		if m.userCanAccessModel(ctx, model, saToken, userContext) {
			authorizedModels = append(authorizedModels, model)
		}
	}

	return authorizedModels, nil
}

// userCanAccessModel checks if the user can access a specific model by making an authorization request.
// Uses the HTTP loopback approach to leverage the gateway's AuthPolicy as the single source of truth.
// userContext is optional but required when using Keycloak to get admin SA token for the user's tier.
func (m *Manager) userCanAccessModel(ctx context.Context, model Model, saToken string, userContext interface{}) bool {
	if model.URL == nil {
		m.logger.Debug("Model URL is nil, denying access", "modelID", model.ID)
		return false
	}

	backoff := wait.Backoff{
		Steps:    4,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
	}

	// Use /v1/chat/completions instead of /v1/models since HTTPRoute doesn't have /v1/models path
	// We'll use OPTIONS method which should work for authorization check
	endpoint, errURL := url.JoinPath(model.URL.String(), "/v1/chat/completions")
	if errURL != nil {
		m.logger.Error("Failed to create endpoint", "modelID", model.ID, "model.URL", model.URL.String(), "errURL", errURL)
		return false
	}

	lastResult := authDenied // fail-closed by default
	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		lastResult = m.doAuthCheck(ctx, endpoint, saToken, model.ID, userContext)
		return lastResult != authRetry, nil
	}); err != nil {
		m.logger.Debug("Authorization check backoff failed", "modelID", model.ID, "error", err)
		return false // explicit fail-closed on error
	}

	return lastResult == authGranted
}

// doAuthCheck performs a single authorization check attempt.
// Uses OPTIONS /v1/chat/completions since HTTPRoute doesn't have /v1/models path.
// OPTIONS method is used because it's a standard HTTP method that should work for authorization checks
// without requiring a request body, and 405 Method Not Allowed indicates auth succeeded.
// For Keycloak: uses the user's Keycloak token directly (Gateway AuthPolicy validates it).
// userContext is optional but required when using Keycloak to determine tier for admin SA token.
func (m *Manager) doAuthCheck(ctx context.Context, authCheckURL, saToken, modelID string, userContext interface{}) authResult {
	// For Keycloak: use the user's Keycloak token directly
	// The Gateway AuthPolicy only accepts Keycloak tokens, so we can't use admin SA tokens
	// The authorization check will verify the user's Keycloak token can access the model endpoint
	// Note: We're relying on Gateway AuthPolicy for authorization, not Kubernetes RBAC
	tokenToUse := saToken
	if m.tokenManager != nil && m.tokenManager.UseKeycloak() {
		// Use the Keycloak token directly - Gateway AuthPolicy will validate it
		m.logger.Debug("Using Keycloak token for model authorization check",
			"modelID", modelID,
		)
	}

	// Use OPTIONS method instead of GET - this works for authorization checks
	// and 405 Method Not Allowed indicates auth succeeded (see comment below)
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, authCheckURL, nil)
	if err != nil {
		m.logger.Debug("Failed to create authorization request", "modelID", modelID, "error", err)
		return authRetry
	}

	req.Header.Set("Authorization", "Bearer "+tokenToUse)

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
		// 404 handling depends on authentication method:
		// - For Keycloak: Gateway AuthPolicy already validated the token, so 404 likely means
		//   routing issue or model not ready, not an auth failure. Grant access since token is valid.
		// - For ServiceAccount tokens: Deny access (fail-closed) as we cannot verify authorization.
		// See: https://issues.redhat.com/browse/RHOAIENG-45883
		if m.tokenManager != nil && m.tokenManager.UseKeycloak() {
			// Keycloak tokens are validated by Gateway AuthPolicy before reaching here
			// If we got a response (even 404), the token passed auth validation
			m.logger.Debug("Model endpoint returned 404, but Keycloak token is valid (Gateway AuthPolicy validated it), granting access", "modelID", modelID)
			return authGranted
		}
		m.logger.Debug("Model endpoint returned 404, denying access (cannot verify authorization)", "modelID", modelID)
		return authDenied

	case resp.StatusCode == http.StatusMethodNotAllowed:
		// 405 Method Not Allowed means the request reached the gateway or model server,
		// proving it passed AuthorizationPolicies (which would return 401/403).
		// The 405 indicates the HTTP method isn't enabled on this route/endpoint,
		// not an authorization failure. Grant access since auth clearly succeeded.
		m.logger.Debug("Model endpoint returned 405 - method not allowed but auth succeeded",
			"modelID", modelID,
			"url", authCheckURL,
		)
		return authGranted

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

// determineTierFromGroups determines the tier name from user groups for PoC hack.
// This is a simple heuristic: look for tier-{tierName}-users groups.
// Returns "free" as default if no tier group is found.
func (m *Manager) determineTierFromGroups(groups []string) string {
	// Look for tier-{tierName}-users pattern in groups
	for _, group := range groups {
		if len(group) > 5 && group[:5] == "tier-" {
			// Extract tier name: "tier-free-users" -> "free"
			parts := strings.Split(group, "-")
			if len(parts) >= 2 {
				return parts[1] // Return the tier name (e.g., "free", "premium", "enterprise")
			}
		}
	}
	// Default to "free" tier if no tier group found
	return "free"
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
