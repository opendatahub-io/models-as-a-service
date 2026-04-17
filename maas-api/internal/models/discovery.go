package models

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/openai/openai-go/v2"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/apis"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type authResult int

const (
	authGranted authResult = iota
	authDenied
	authRetry
)

const maxModelsResponseBytes int64 = 4 << 20 // 4 MiB

// HTTP client and concurrency for access-validation probes.
const (
	httpMaxIdleConns        = 100
	httpIdleConnTimeout     = 90 * time.Second
	maxDiscoveryConcurrency = 10

	// defaultAccessCheckTimeout bounds the total duration of FilterModelsByAccess.
	// This limits the staleness window between when access is checked and when
	// the response reaches the client. Models whose probes don't complete within
	// this window are excluded (fail-closed).
	defaultAccessCheckTimeout = 15 * time.Second
)

// Manager runs access validation (probe model endpoints) for models listed from MaaSModelRef.
type Manager struct {
	logger             *logger.Logger
	httpClient         *http.Client
	accessCheckTimeout time.Duration
}

// NewManager creates a Manager for filtering models by access. The client uses InsecureSkipVerify
// for cluster-internal probes; auth is enforced by the gateway/model server.
// accessCheckTimeoutSeconds controls the total duration bound for access validation;
// if <= 0, the default of 15 seconds is used.
func NewManager(log *logger.Logger, accessCheckTimeoutSeconds int) (*Manager, error) {
	if log == nil {
		return nil, errors.New("log is required")
	}
	timeout := defaultAccessCheckTimeout
	if accessCheckTimeoutSeconds > 0 {
		timeout = time.Duration(accessCheckTimeoutSeconds) * time.Second
	}
	return &Manager{
		logger:             log,
		accessCheckTimeout: timeout,
		httpClient: &http.Client{
			// No per-client Timeout — each request inherits the accessCheckTimeout
			// deadline via its context. This ensures that configuring a longer
			// ACCESS_CHECK_TIMEOUT_SECONDS actually allows slower backends to respond.
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // cluster-internal only
				MaxIdleConns:        httpMaxIdleConns,
				MaxIdleConnsPerHost: maxDiscoveryConcurrency,
				IdleConnTimeout:     httpIdleConnTimeout,
			},
		},
	}, nil
}

// FilterModelsByAccess returns only models the user can access by probing each model's
// /v1/models endpoint with the given Authorization and x-maas-subscription headers (passed through as-is).
// 2xx or 405 → include, 401/403/404 → exclude.
// Models with nil URL are skipped. Concurrency is limited by maxDiscoveryConcurrency.
//
// Because authorization policies propagate asynchronously through the gateway, there is an
// inherent eventual-consistency window: a model listed here may become inaccessible (or vice versa)
// by the time the client acts on the response. Actual enforcement always happens at the gateway
// when the model is invoked for inference. Callers should set Cache-Control: no-store and expose
// a freshness timestamp via response headers so clients can assess freshness.
//
// The access check is bounded by accessCheckTimeout to limit the staleness window.
func (m *Manager) FilterModelsByAccess(ctx context.Context, models []Model, authHeader string, subscriptionHeader string) []Model {
	if len(models) == 0 {
		return models
	}

	// Bound the total access-check duration to limit the staleness window.
	ctx, cancel := context.WithTimeout(ctx, m.accessCheckTimeout)
	defer cancel()

	m.logger.Debug("FilterModelsByAccess: validating access for models", "count", len(models), "subscriptionHeaderProvided", subscriptionHeader != "")
	// Initialize to empty slice (not nil) so JSON marshals as [] instead of null when no models are accessible
	out := []Model{}
	var mu sync.Mutex
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxDiscoveryConcurrency)
	for i := range models {
		model := models[i]
		// External models cannot be probed — their /v1/models endpoint requires
		// the provider API key (injected by BBR), not the user's MaaS token.
		// Include them directly if they are Ready; access is enforced by the
		// gateway auth policy at inference time.
		if model.Kind == "ExternalModel" {
			if model.Ready {
				m.logger.Debug("FilterModelsByAccess: including external model (no probe)", "id", model.ID)
				mu.Lock()
				out = append(out, model)
				mu.Unlock()
			} else {
				m.logger.Debug("FilterModelsByAccess: skipping external model (not ready)", "id", model.ID)
			}
			continue
		}
		if model.URL == nil {
			m.logger.Debug("FilterModelsByAccess: skipping model with no URL", "id", model.ID)
			continue
		}
		modelsEndpoint, err := url.JoinPath(model.URL.String(), "v1", "models")
		if err != nil {
			m.logger.Debug("FilterModelsByAccess: failed to build endpoint", "id", model.ID, "error", err)
			continue
		}
		kind := model.Kind
		if kind == "" {
			kind = "llmisvc"
		}
		meta := modelMetadata{
			Kind:        kind,
			ServiceName: model.ID,
			ModelName:   model.ID,
			Endpoint:    modelsEndpoint,
			URL:         model.URL,
			Ready:       model.Ready,
			Namespace:   model.OwnedBy,
			Created:     model.Created,
		}
		g.Go(func() error {
			if discovered := m.fetchModelsWithRetry(ctx, authHeader, subscriptionHeader, meta); discovered != nil {
				// Use model names from the backend's /v1/models response instead of MaaSModelRef metadata.name
				converted := discoveredToModels(discovered, model)
				mu.Lock()
				out = append(out, converted...)
				mu.Unlock()
				for _, c := range converted {
					m.logger.Debug("FilterModelsByAccess: access granted", "model", c.ID, "endpoint", modelsEndpoint)
				}
			} else {
				m.logger.Debug("FilterModelsByAccess: access denied or unreachable", "model", model.ID, "endpoint", modelsEndpoint)
			}
			return nil
		})
	}
	_ = g.Wait()
	m.logger.Debug("FilterModelsByAccess: complete", "input", len(models), "accessible", len(out))
	return out
}

// Access validation: we only include a model if a GET to that model's /v1/models endpoint
// with the request's Authorization header (passed through as-is) succeeds. So we "validate access
// by making a call": same gateway/auth path as inference. fetchModels does GET meta.Endpoint
// with the given Authorization header; 2xx or 405 → include (authGranted), 401/403/404 → exclude (authDenied),
// 5xx/other → retry (authRetry).

// discoveredToModels converts backend /v1/models response to our Model type, using the backend's
// model names (id) and preserving URL, Ready, Kind from the original MaaSModelRef-derived model.
// If the backend returns no models, falls back to the original model (MaaSModelRef metadata.name).
func discoveredToModels(discovered []openai.Model, original Model) []Model {
	if len(discovered) == 0 {
		return []Model{original}
	}
	out := make([]Model, 0, len(discovered))
	for _, d := range discovered {
		if d.ID == "" {
			continue
		}
		// Always use the trusted namespace from MaaSModelRef (original.OwnedBy)
		// Never trust backend-returned OwnedBy to prevent namespace spoofing
		created := d.Created
		if created == 0 {
			created = original.Created
		}
		out = append(out, Model{
			Model: openai.Model{
				ID:      d.ID,
				Object:  "model",
				Created: created,
				OwnedBy: original.OwnedBy,
			},
			Kind:    original.Kind,
			URL:     original.URL,
			Ready:   original.Ready,
			Details: original.Details,
		})
	}
	// Fallback: if backend returned items but all had empty IDs, use original model
	if len(out) == 0 {
		return []Model{original}
	}
	return out
}

// modelMetadata holds the data needed to probe a model endpoint and to enrich the response when applicable.
type modelMetadata struct {
	Kind        string    // model ref kind, e.g. "llmisvc" (from MaaSModelRef spec.modelRef.kind)
	ServiceName string    // for logging and error messages
	ModelName   string    // model id for 405 fallback response
	Endpoint    string    // full URL to GET (e.g. base + /v1/models)
	URL         *apis.URL // base URL (for enrichModel when used)
	Ready       bool
	Details     *Details
	Namespace   string
	Created     int64
}

func (m *Manager) fetchModelsWithRetry(ctx context.Context, authHeader string, subscriptionHeader string, meta modelMetadata) []openai.Model {
	m.logger.Debug("Validating access: probing model endpoint",
		"service", meta.ServiceName,
		"endpoint", meta.Endpoint,
		"kind", meta.Kind,
		"subscriptionHeaderProvided", subscriptionHeader != "",
	)
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
		models, authRes = m.fetchModels(ctx, authHeader, subscriptionHeader, meta)
		if authRes == authGranted {
			result = models
		}
		lastResult = authRes
		return lastResult != authRetry, nil
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			m.logger.Debug("Access validation failed: context deadline exceeded", "service", meta.ServiceName, "endpoint", meta.Endpoint, "timeout", m.accessCheckTimeout)
		} else {
			m.logger.Debug("Access validation failed: model fetch backoff exhausted", "service", meta.ServiceName, "endpoint", meta.Endpoint, "error", err)
		}
		return nil // explicit fail-closed on error
	}

	if lastResult != authGranted {
		m.logger.Debug("Access validation denied for model", "service", meta.ServiceName, "endpoint", meta.Endpoint)
		return nil
	}
	m.logger.Debug("Access validation granted for model", "service", meta.ServiceName, "endpoint", meta.Endpoint)
	return result
}

func (m *Manager) fetchModels(ctx context.Context, authHeader string, subscriptionHeader string, meta modelMetadata) ([]openai.Model, authResult) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.Endpoint, nil)
	if err != nil {
		m.logger.Debug("Access validation: failed to create GET request", "service", meta.ServiceName, "endpoint", meta.Endpoint, "error", err)
		return nil, authRetry
	}

	req.Header.Set("Authorization", authHeader)
	if subscriptionHeader != "" {
		req.Header.Set("X-Maas-Subscription", subscriptionHeader)
	}

	// #nosec G704 -- Intentional HTTP request to probe model endpoint for authorization check
	resp, err := m.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			m.logger.Debug("Access validation: request timed out (context deadline exceeded)", "service", meta.ServiceName, "endpoint", meta.Endpoint)
			return nil, authDenied // fail-closed, no point retrying a deadline
		}
		m.logger.Debug("Access validation: GET request failed", "service", meta.ServiceName, "endpoint", meta.Endpoint, "error", err)
		return nil, authRetry
	}
	defer resp.Body.Close()

	m.logger.Debug("Access validation: model endpoint response",
		"service", meta.ServiceName,
		"endpoint", meta.Endpoint,
		"statusCode", resp.StatusCode,
		"authHeaderLen", len(authHeader),
		"subscriptionHeaderProvided", subscriptionHeader != "",
	)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(body) > 0 {
			m.logger.Debug("Access validation: auth failure response body", "service", meta.ServiceName, "endpoint", meta.Endpoint, "bodyPreview", string(body))
		}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		models, parseErr := m.parseModelsResponse(resp.Body, meta)
		if parseErr != nil {
			m.logger.Debug("Failed to parse models response", "service", meta.ServiceName, "error", parseErr)
			return nil, authRetry
		}
		return models, authGranted

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		m.logger.Debug("Access validation: endpoint returned auth failure", "service", meta.ServiceName, "endpoint", meta.Endpoint, "statusCode", resp.StatusCode)
		return nil, authDenied

	case resp.StatusCode == http.StatusNotFound:
		// 404 means we cannot verify authorization - deny access (fail-closed)
		// See: https://issues.redhat.com/browse/RHOAIENG-45883
		m.logger.Debug("Access validation: endpoint returned 404, denying access (cannot verify authorization)", "service", meta.ServiceName, "endpoint", meta.Endpoint)
		return nil, authDenied

	case resp.StatusCode == http.StatusMethodNotAllowed:
		// 405 Method Not Allowed means the request reached the gateway or model server,
		// proving it passed AuthorizationPolicies (which would return 401/403).
		// The 405 indicates the HTTP method isn't enabled on this route/endpoint,
		// not an authorization failure.
		m.logger.Debug("Model endpoint returned 405 - auth succeeded, using model name as fallback ID",
			"service", meta.ServiceName,
			"modelName", meta.ModelName,
			"endpoint", meta.Endpoint,
		)
		return []openai.Model{{
			ID:     meta.ModelName,
			Object: "model",
		}}, authGranted

	default:
		// Retry on server errors (5xx) or other unexpected codes
		m.logger.Debug("Access validation: unexpected status code, will retry",
			"service", meta.ServiceName,
			"endpoint", meta.Endpoint,
			"statusCode", resp.StatusCode,
		)
		return nil, authRetry
	}
}

func (m *Manager) parseModelsResponse(body io.Reader, meta modelMetadata) ([]openai.Model, error) {
	// Read max+1 so we can detect "over limit" instead of silently truncating.
	limited := io.LimitReader(body, maxModelsResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("service %s (%s): failed to read response body: %w", meta.ServiceName, meta.Endpoint, err)
	}
	if int64(len(data)) > maxModelsResponseBytes {
		return nil, fmt.Errorf("service %s (%s): models response too large (> %d bytes)", meta.ServiceName, meta.Endpoint, maxModelsResponseBytes)
	}

	var response struct {
		Data []openai.Model `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("service %s (%s): failed to unmarshal models response: %w", meta.ServiceName, meta.Endpoint, err)
	}

	m.logger.Debug("Discovered models from service",
		"service", meta.ServiceName,
		"endpoint", meta.Endpoint,
		"modelCount", len(response.Data),
	)

	return response.Data, nil
}
