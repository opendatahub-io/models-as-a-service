package metrics

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
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
		if !slices.Contains(tlsCfg.NextProtos, "h2") {
			disableHTTP2(srv)
		}
	}

	return srv, nil
}

// disableHTTP2 prevents net/http from running its HTTP/2 setup on a server
// whose TLS configuration cannot serve it. NextProtos alone is not enough:
// ListenAndServeTLS runs the HTTP/2 startup check before applying ALPN, and
// that check rejects TLS 1.2 cipher lists without an ECDHE AES-128-GCM suite.
func disableHTTP2(srv *http.Server) {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	srv.Protocols = protocols
}

type metricsCertLoader struct {
	mu       sync.Mutex
	certFile string
	keyFile  string
}

func (l *metricsCertLoader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cert, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading metrics TLS certificate from %s: %w", filepath.Dir(l.certFile), err)
	}
	return &cert, nil
}

func buildMetricsTLSConfig(certDir string, profileMinVersion uint16, profileCipherSuites []uint16) (*tls.Config, error) {
	loader := &metricsCertLoader{
		certFile: filepath.Join(certDir, "tls.crt"),
		keyFile:  filepath.Join(certDir, "tls.key"),
	}
	if _, err := loader.getCertificate(nil); err != nil {
		return nil, err
	}

	minVersion := uint16(tls.VersionTLS12)
	if profileMinVersion > 0 {
		minVersion = profileMinVersion
	}

	tlsCfg := &tls.Config{
		GetCertificate: loader.getCertificate,
		MinVersion:     minVersion,
		NextProtos:     metricsNextProtos(minVersion, profileCipherSuites),
	}
	if len(profileCipherSuites) > 0 {
		tlsCfg.CipherSuites = profileCipherSuites
	}
	return tlsCfg, nil
}

func metricsNextProtos(minVersion uint16, cipherSuites []uint16) []string {
	if minVersion < tls.VersionTLS12 {
		return []string{"http/1.1"}
	}
	// TLS 1.3 and an empty TLS 1.2 cipher list both use Go's defaults, which
	// include an HTTP/2-eligible ECDHE AES-128-GCM suite.
	if minVersion >= tls.VersionTLS13 || len(cipherSuites) == 0 {
		return []string{"h2", "http/1.1"}
	}
	for _, id := range cipherSuites {
		if id == tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 ||
			id == tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
			return []string{"h2", "http/1.1"}
		}
	}
	return []string{"http/1.1"}
}

// ListenAndServe starts the metrics server (HTTPS when TLSConfig is set).
func ListenAndServe(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}
