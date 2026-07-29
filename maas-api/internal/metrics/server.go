package metrics

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

// ServerOptions configures the standalone Prometheus metrics HTTP(S) server.
type ServerOptions struct {
	Addr string
	Reg  *prometheus.Registry

	// Secure serves /metrics over HTTPS with kube authn/authz when true.
	Secure bool
	// CertDir contains tls.crt and tls.key (OpenShift service-ca mount).
	CertDir string
	// RESTConfig is required when Secure is true (TokenReview / SAR).
	RESTConfig *rest.Config
	// HTTPClient is used by the authn/authz filter; optional (rest.HTTPClientFor used if nil).
	HTTPClient *http.Client
	// Logger is passed to the metrics auth filter.
	Logger logr.Logger

	// ProfileMinVersion / ProfileCipherSuites come from the cluster TLS profile.
	ProfileMinVersion   uint16
	ProfileCipherSuites []uint16
}

// NewMetricsServer builds an http.Server that exposes /metrics.
// When opts.Secure is true, the handler requires a bearer token authorized for
// GET /metrics and TLS is configured from CertDir (plus optional cluster profile).
func NewMetricsServer(opts ServerOptions) (*http.Server, error) {
	if opts.Reg == nil {
		return nil, errors.New("prometheus registry is required")
	}
	if err := opts.Reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("registering Go collector: %w", err)
	}

	handler := promhttp.HandlerFor(opts.Reg, promhttp.HandlerOpts{})

	if opts.Secure {
		if opts.RESTConfig == nil {
			return nil, errors.New("REST config is required when metrics are served securely")
		}
		if opts.CertDir == "" {
			return nil, errors.New("cert dir is required when metrics are served securely")
		}

		httpClient := opts.HTTPClient
		if httpClient == nil {
			var err error
			httpClient, err = rest.HTTPClientFor(opts.RESTConfig)
			if err != nil {
				return nil, fmt.Errorf("creating HTTP client for metrics auth: %w", err)
			}
		}

		filter, err := filters.WithAuthenticationAndAuthorization(opts.RESTConfig, httpClient)
		if err != nil {
			return nil, fmt.Errorf("creating metrics auth filter: %w", err)
		}
		log := opts.Logger
		if log.IsZero() {
			log = logr.Discard()
		}
		handler, err = filter(log, handler)
		if err != nil {
			return nil, fmt.Errorf("wrapping metrics handler with auth filter: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	if opts.Secure {
		tlsCfg, err := buildMetricsTLSConfig(opts.CertDir, opts.ProfileMinVersion, opts.ProfileCipherSuites)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = tlsCfg
	}

	return srv, nil
}

func buildMetricsTLSConfig(certDir string, profileMinVersion uint16, profileCipherSuites []uint16) (*tls.Config, error) {
	certFile := filepath.Join(certDir, "tls.crt")
	keyFile := filepath.Join(certDir, "tls.key")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading metrics TLS certificate from %s: %w", certDir, err)
	}

	minVersion := uint16(tls.VersionTLS12)
	if profileMinVersion > 0 {
		minVersion = profileMinVersion
	}

	//nolint:gosec // G402: MinVersion comes from cluster TLS profile or TLS 1.2 default
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if len(profileCipherSuites) > 0 {
		tlsCfg.CipherSuites = profileCipherSuites
	}
	return tlsCfg, nil
}

// ListenAndServe starts the metrics server (HTTPS when TLSConfig is set).
func ListenAndServe(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}
