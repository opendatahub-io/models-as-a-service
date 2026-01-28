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

const maxModelsResponseBytes int64 = 4 << 20 // 4 MiB

type llmInferenceServiceMetadata struct {
	ServiceName    string // LLMInferenceService resource name (for logging)
	ModelName      string // from spec.model.name or fallback to service name
	URL            *apis.URL
	ModelsEndpoint string // full URL to /v1/models endpoint
	Ready          bool
	Details        *Details
	Namespace      string
	Created        int64
}

func (m *Manager) fetchModelsWithRetry(ctx context.Context, saToken string, svc llmInferenceServiceMetadata) []openai.Model {
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
		models, authRes = m.fetchModels(ctx, saToken, svc)
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

func (m *Manager) fetchModels(ctx context.Context, saToken string, svc llmInferenceServiceMetadata) ([]openai.Model, authResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.ModelsEndpoint, nil)
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
		"endpoint", svc.ModelsEndpoint,
	)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		models, parseErr := m.parseModelsResponse(resp.Body, svc)
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
			"endpoint", svc.ModelsEndpoint,
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

func (m *Manager) parseModelsResponse(body io.Reader, svc llmInferenceServiceMetadata) ([]openai.Model, error) {
	// Read max+1 so we can detect "over limit" instead of silently truncating.
	limited := io.LimitReader(body, maxModelsResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("service %s (%s): failed to read response body: %w", svc.ServiceName, svc.ModelsEndpoint, err)
	}
	if int64(len(data)) > maxModelsResponseBytes {
		return nil, fmt.Errorf("service %s (%s): models response too large (> %d bytes)", svc.ServiceName, svc.ModelsEndpoint, maxModelsResponseBytes)
	}

	var response struct {
		Data []openai.Model `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("service %s (%s): failed to unmarshal models response: %w", svc.ServiceName, svc.ModelsEndpoint, err)
	}

	m.logger.Debug("Discovered models from service",
		"service", svc.ServiceName,
		"endpoint", svc.ModelsEndpoint,
		"modelCount", len(response.Data),
	)

	return response.Data, nil
}

func (m *Manager) enrichModel(discovered []openai.Model, llmIsvcMetadata llmInferenceServiceMetadata) *Model {
	if len(discovered) == 0 {
		return nil
	}

	model := Model{
		Model:   discovered[0],
		URL:     llmIsvcMetadata.URL,
		Ready:   llmIsvcMetadata.Ready,
		Details: llmIsvcMetadata.Details,
	}
	if model.OwnedBy == "" {
		model.OwnedBy = llmIsvcMetadata.Namespace
	}
	if model.Created == 0 {
		model.Created = llmIsvcMetadata.Created
	}

	for i := 1; i < len(discovered); i++ {
		model.Aliases = append(model.Aliases, discovered[i].ID)
	}

	return &model
}
