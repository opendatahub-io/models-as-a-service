package models

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// HTTP client transport configuration for the access check probe (kept for TLS bootstrap).
const (
	httpMaxIdleConns          = 100
	httpIdleConnTimeout       = 90 * time.Second
	maxDiscoveryConcurrency   = 10
	defaultAccessCheckTimeout = 15 * time.Second
)

// kubeServiceAccountCAPath is the path to the Kubernetes service account CA certificate.
// This CA is used to validate TLS certificates for cluster-internal services.
const kubeServiceAccountCAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// Manager runs access validation (probe model endpoints) for models listed from MaaSModelRef.
type Manager struct {
	logger              *logger.Logger
	httpClient          *http.Client
	accessCheckTimeout  time.Duration
	gatewayInternalHost string
}

// NewManager creates a Manager for filtering models by access.
// The client uses proper TLS certificate validation via the Kubernetes service account CA
// (when running in-cluster) or system root CAs (when running locally).
// accessCheckTimeoutSeconds controls the total duration bound for access validation;
// if <= 0, the default of 15 seconds is used.
// gatewayInternalHost, when non-empty, routes all probe TCP connections to this
// cluster-internal address while preserving the original URL hostname for TLS SNI
// and the Host header, so gateway routing and Authorino auth work identically.
func NewManager(log *logger.Logger, accessCheckTimeoutSeconds int, gatewayInternalHost string, enableHTTP2 bool) (*Manager, error) {
	if log == nil {
		return nil, errors.New("log is required")
	}
	timeout := defaultAccessCheckTimeout
	if accessCheckTimeoutSeconds > 0 {
		timeout = time.Duration(accessCheckTimeoutSeconds) * time.Second
	}

	tlsConfig, err := BuildClusterTLSConfigFromPath(log, kubeServiceAccountCAPath, enableHTTP2)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS config: %w", err)
	}

	// No per-client Timeout — each request inherits the accessCheckTimeout
	// deadline via its context. This ensures that configuring a longer
	// ACCESS_CHECK_TIMEOUT_SECONDS actually allows slower backends to respond.
	transport := &http.Transport{
		TLSClientConfig:     tlsConfig,
		MaxIdleConns:        httpMaxIdleConns,
		MaxIdleConnsPerHost: maxDiscoveryConcurrency,
		IdleConnTimeout:     httpIdleConnTimeout,
	}

	if gatewayInternalHost != "" {
		dialer := &net.Dialer{}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				port = "443"
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(gatewayInternalHost, port))
		}
	}

	return &Manager{
		logger:              log,
		accessCheckTimeout:  timeout,
		gatewayInternalHost: gatewayInternalHost,
		httpClient: &http.Client{
			Transport: transport,
		},
	}, nil
}

// BuildClusterTLSConfig creates a TLS config for cluster-internal communication using
// the default Kubernetes service account CA path. It is a convenience wrapper around
// BuildClusterTLSConfigFromPath.
func BuildClusterTLSConfig(log *logger.Logger) (*tls.Config, error) {
	return BuildClusterTLSConfigFromPath(log, kubeServiceAccountCAPath, false)
}

// BuildClusterTLSConfigFromPath creates a TLS config for cluster-internal communication.
// It starts with the system root CAs and appends the CA certificate at caPath when present.
// This ensures both public CAs and cluster CAs are trusted, supporting endpoints with
// publicly-trusted certificates as well as cluster-internal services.
//
// If caPath does not exist, system root CAs are used alone (development/out-of-cluster mode).
// If caPath exists but cannot be read or parsed, an error is returned to prevent insecure fallback.
func BuildClusterTLSConfigFromPath(log *logger.Logger, caPath string, enableHTTP2 bool) (*tls.Config, error) {
	if log == nil {
		return nil, errors.New("log is required")
	}

	caCertPool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("failed to load system certificate pool: %w", err)
	}

	caCert, err := os.ReadFile(caPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Debug("Kubernetes service account CA not found, using system root CAs only",
				"path", caPath)
		} else {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
	} else {
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, errors.New("failed to parse Kubernetes service account CA certificate")
		}
		log.Debug("Using system root CAs with Kubernetes service account CA appended",
			"path", caPath)
	}

	tlsCfg := &tls.Config{
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS12,
	}
	if enableHTTP2 {
		tlsCfg.NextProtos = []string{"h2", "http/1.1"}
	}
	return tlsCfg, nil
}

// FilterModelsByAccess returns only models the user is entitled to see.
//
// Access enforcement for all known model kinds happens at inference time via the gateway
// authpolicy. The discovery response relies on subscription pre-filtering (in ListLLMs)
// as the primary access boundary.
//
// Kind semantics (exhaustive — only these three are valid):
//   - "ExternalModel": included directly if Ready; backend cannot be probed because the
//     provider API key is injected by IPP, not carried in the user token.
//   - "llmisvc" / "LLMInferenceService" / "" (default): included directly if Ready;
//     BBR clusters share a single gateway base URL so per-model probing is not meaningful.
func (m *Manager) FilterModelsByAccess(_ context.Context, models []Model, _ string, _ string) []Model {
	if len(models) == 0 {
		return models
	}

	m.logger.Debug("FilterModelsByAccess: filtering models by readiness", "count", len(models))
	// Initialize to empty slice (not nil) so JSON marshals as [] instead of null.
	out := make([]Model, 0, len(models))
	for _, model := range models {
		switch model.Kind {
		case kindExternalModel:
			// ExternalModel endpoints require the provider API key injected by IPP;
			// probing is not possible with the user's MaaS token.
			if model.Ready {
				m.logger.Debug("FilterModelsByAccess: including ExternalModel (no probe)", "id", model.ID)
				out = append(out, model)
			} else {
				m.logger.Debug("FilterModelsByAccess: skipping ExternalModel (not ready)", "id", model.ID)
			}
		case kindLLMISvc, kindLLMISvcAlternate, "":
			// Both kindLLMISvc ("llmisvc") and kindLLMISvcAlternate ("LLMInferenceService") are
			// valid values for MaaSModelRef spec.modelRef.kind; empty defaults to kindLLMISvc.
			if model.Ready {
				m.logger.Debug("FilterModelsByAccess: including LLMInferenceService (no probe)", "id", model.ID)
				out = append(out, model)
			} else {
				m.logger.Debug("FilterModelsByAccess: skipping LLMInferenceService (not ready)", "id", model.ID)
			}
		default:
			m.logger.Debug("FilterModelsByAccess: skipping model with unknown kind", "id", model.ID, "kind", model.Kind)
		}
	}
	m.logger.Debug("FilterModelsByAccess: complete", "input", len(models), "accessible", len(out))
	return out
}
