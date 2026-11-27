package metrics

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// metricsAuthzVerb and metricsAuthzURL define the permission a scraper must
// hold: "get" on the /metrics non-resource URL, granted via a ClusterRole
// nonResourceURLs rule — the same convention maas-api and controller-runtime
// use.
const (
	metricsAuthzVerb = "get"
	metricsAuthzURL  = "/metrics"
)

// ServerOptions configures the standalone Prometheus metrics HTTP(S) server.
type ServerOptions struct {
	// Addr is the listen address.
	Addr string

	// Secure serves /metrics over HTTPS with kube authn/authz when true.
	Secure bool
	// CertDir contains tls.crt and tls.key (OpenShift service-ca mount);
	// required when Secure is true.
	CertDir string
	// KubeClient authenticates and authorizes scrapers via TokenReview and
	// SubjectAccessReview; required when Secure is true.
	KubeClient kubernetes.Interface
	// Log is used for auth failures; optional.
	Log *slog.Logger

	// MinVersion and CipherSuites come from the cluster TLS profile,
	// matching the main server.
	MinVersion   uint16
	CipherSuites []uint16
}

// NewServer builds the http.Server exposing /metrics. When opts.Secure is
// true the handler requires a bearer token that is TokenReview-authenticated
// and SAR-authorized for GET /metrics, and TLS is configured from CertDir
// plus the cluster profile. Failures fail closed.
func (m *Metrics) NewServer(opts ServerOptions) (*http.Server, error) {
	if m == nil {
		return nil, errors.New("metrics is required")
	}

	handler := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})

	if opts.Secure {
		if opts.KubeClient == nil {
			return nil, errors.New("kubernetes client is required when metrics are served securely")
		}
		if opts.CertDir == "" {
			return nil, errors.New("cert dir is required when metrics are served securely")
		}
		handler = metricsAuthFilter(opts.Log, opts.KubeClient, handler)
	}

	mux := http.NewServeMux()
	mux.Handle(metricsAuthzURL, handler)

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	if opts.Secure {
		tlsCfg, err := buildMetricsTLSConfig(opts.CertDir, opts.MinVersion, opts.CipherSuites)
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

// metricsAuthFilter wraps the metrics handler with bearer-token
// authentication (TokenReview) and authorization (SubjectAccessReview for
// GET /metrics). Any error fails closed.
func metricsAuthFilter(log *slog.Logger, kubeClient kubernetes.Interface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if log == nil {
			log = slog.Default()
		}

		token := bearerToken(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		tr, err := kubeClient.AuthenticationV1().TokenReviews().Create(r.Context(),
			&authv1.TokenReview{
				Spec: authv1.TokenReviewSpec{Token: token},
			}, metav1.CreateOptions{})
		if err != nil {
			log.Error("metrics TokenReview failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !tr.Status.Authenticated {
			log.Warn("metrics scrape rejected: token not authenticated",
				"user", tr.Status.User.Username)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		sar, err := kubeClient.AuthorizationV1().SubjectAccessReviews().Create(r.Context(),
			&authzv1.SubjectAccessReview{
				Spec: authzv1.SubjectAccessReviewSpec{
					User:   tr.Status.User.Username,
					Groups: tr.Status.User.Groups,
					NonResourceAttributes: &authzv1.NonResourceAttributes{
						Path: metricsAuthzURL,
						Verb: metricsAuthzVerb,
					},
				},
			}, metav1.CreateOptions{})
		if err != nil {
			log.Error("metrics SubjectAccessReview failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !sar.Status.Allowed {
			log.Warn("metrics scrape rejected: not authorized",
				"user", tr.Status.User.Username)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
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

// metricsCertLoader reloads the serving certificate on every handshake so
// service-ca rotations are picked up without a restart.
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
		// Go's net/http requires an ECDHE AES-128-GCM suite for HTTP/2;
		// RSA key exchange suites do not qualify.
		if id == tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 ||
			id == tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
			return []string{"h2", "http/1.1"}
		}
	}
	return []string{"http/1.1"}
}
