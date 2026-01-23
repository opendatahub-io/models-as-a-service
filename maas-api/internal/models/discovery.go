package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openai/openai-go/v2"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/apis"
)

type authResult int

const (
	authGranted authResult = iota
	authDenied
	authRetry
)

type llmInferenceServiceMetadata struct {
	ServiceName string // LLMInferenceService resource name (for logging)
	ModelName   string // from spec.model.name or fallback to service name
	URL         *apis.URL
	Ready       bool
	Details     *Details
	Namespace   string
	Created     int64
}

func (m *Manager) fetchModelsWithRetry(ctx context.Context, endpoint, saToken string, svc llmInferenceServiceMetadata) []openai.Model {
	backoff := wait.Backoff{
		Steps:    4,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
	}

	var result []openai.Model
	lastResult := authDenied // fail-closed by default

	if err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		var models []openai.Model
		var authRes authResult
		models, authRes = m.fetchModels(ctx, endpoint, saToken, svc)
		if authRes == authGranted {
			result = models
		}
		lastResult = authRes
		return lastResult != authRetry, nil
	}); err != nil {
		m.logger.Debug("Model fetch backoff failed", "service", svc.ServiceName, "error", err)
		return nil // explicit fail-closed on error
	}

	if lastResult != authGranted {
		return nil
	}
	return result
}

func (m *Manager) fetchModels(ctx context.Context, endpoint, saToken string, svc llmInferenceServiceMetadata) ([]openai.Model, authResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		m.logger.Debug("Failed to create request", "service", svc.ServiceName, "error", err)
		return nil, authRetry
	}

	req.Header.Set("Authorization", "Bearer "+saToken)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		m.logger.Debug("Request failed", "service", svc.ServiceName, "error", err)
		return nil, authRetry
	}
	defer resp.Body.Close()

	m.logger.Debug("Models fetch response",
		"service", svc.ServiceName,
		"statusCode", resp.StatusCode,
		"url", endpoint,
	)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		models, parseErr := m.parseModelsResponse(resp.Body, svc.ServiceName)
		if parseErr != nil {
			m.logger.Debug("Failed to parse models response", "service", svc.ServiceName, "error", parseErr)
			return nil, authRetry
		}
		return models, authGranted

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, authDenied

	case resp.StatusCode == http.StatusNotFound:
		// 404 means we cannot verify authorization - deny access (fail-closed)
		// See: https://issues.redhat.com/browse/RHOAIENG-45883
		m.logger.Debug("Model endpoint returned 404, denying access (cannot verify authorization)", "service", svc.ServiceName)
		return nil, authDenied

	case resp.StatusCode == http.StatusMethodNotAllowed:
		// 405 Method Not Allowed means the request reached the gateway or model server,
		// proving it passed AuthorizationPolicies (which would return 401/403).
		// The 405 indicates the HTTP method isn't enabled on this route/endpoint,
		// not an authorization failure.
		// Use spec.model.name as a best-effort fallback for model ID.
		m.logger.Debug("Model endpoint returned 405 - auth succeeded, using spec.model.name as fallback model ID",
			"service", svc.ServiceName,
			"modelName", svc.ModelName,
			"url", endpoint,
		)
		return []openai.Model{{
			ID:     svc.ModelName,
			Object: "model",
		}}, authGranted

	default:
		// Retry on server errors (5xx) or other unexpected codes
		m.logger.Debug("Unexpected status code, retrying",
			"service", svc.ServiceName,
			"statusCode", resp.StatusCode,
		)
		return nil, authRetry
	}
}

func (m *Manager) parseModelsResponse(body io.Reader, llmIsvcName string) ([]openai.Model, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var response struct {
		Data []openai.Model `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal models response: %w", err)
	}

	m.logger.Debug("Discovered models from service",
		"llmIsvcName", llmIsvcName,
		"modelCount", len(response.Data),
	)

	return response.Data, nil
}

func (m *Manager) enrichModels(discovered []openai.Model, svcMetadata llmInferenceServiceMetadata) []Model {
	models := make([]Model, 0, len(discovered))
	for _, dm := range discovered {
		model := Model{
			Model:   dm,
			URL:     svcMetadata.URL,
			Ready:   svcMetadata.Ready,
			Details: svcMetadata.Details,
		}
		if model.OwnedBy == "" {
			model.OwnedBy = svcMetadata.Namespace
		}
		if model.Created == 0 {
			model.Created = svcMetadata.Created
		}
		models = append(models, model)
	}
	return models
}
