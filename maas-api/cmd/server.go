package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/cert"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
)

func newServer(cfg *config.Config, handler http.Handler, profileMinVersion uint16, profileCipherSuites []uint16) (*http.Server, error) {
	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	if cfg.Secure {
		tlsConfig, err := buildTLSConfig(cfg, profileMinVersion, profileCipherSuites)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = tlsConfig
		if !slices.Contains(tlsConfig.NextProtos, "h2") {
			disableHTTP2(srv)
		}
	}

	return srv, nil
}

// disableHTTP2 prevents net/http from running its HTTP/2 setup on a server
// whose TLS configuration cannot serve it. NextProtos alone is not enough:
// ServeTLS runs the HTTP/2 startup check before applying ALPN, and that check
// rejects TLS 1.2 cipher lists without an ECDHE AES-128-GCM suite.
func disableHTTP2(srv *http.Server) {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	srv.Protocols = protocols
}

// buildTLSConfig creates the TLS configuration for the API server.
// When profileMinVersion > 0 and profileCipherSuites are provided (from the
// cluster's TLS security profile), they take precedence over the flag-based
// MinVersion. This ensures the server honors the cluster-wide TLS policy on
// OpenShift while falling back to flag-driven defaults on vanilla Kubernetes.
func buildTLSConfig(cfg *config.Config, profileMinVersion uint16, profileCipherSuites []uint16) (*tls.Config, error) {
	var tlsCert tls.Certificate
	var err error

	if cfg.TLS.HasCerts() {
		tlsCert, err = tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
	} else if cfg.TLS.SelfSigned {
		tlsCert, err = cert.Generate(cfg.Name)
		if err != nil {
			return nil, fmt.Errorf("generating self-signed certificate: %w", err)
		}
	}

	minVersion := cfg.TLS.MinVersion.Value()
	if profileMinVersion > 0 {
		minVersion = profileMinVersion
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   minVersion,
		NextProtos:   tlsNextProtos(minVersion, profileCipherSuites),
	}

	if len(profileCipherSuites) > 0 {
		tlsCfg.CipherSuites = profileCipherSuites
	}

	return tlsCfg, nil
}

func tlsNextProtos(minVersion uint16, cipherSuites []uint16) []string {
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

func listenAndServe(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}
