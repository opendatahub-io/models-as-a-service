package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/cert"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
)

func newServer(cfg *config.Config, handler http.Handler) (*http.Server, error) {
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
		tlsConfig, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = tlsConfig
	}

	return srv, nil
}

func buildTLSConfig(cfg *config.Config) (*tls.Config, error) {
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

	//nolint:gosec // G402: MinVersion is configurable via --tls-min-version flag (default: TLS 1.2)
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   cfg.TLS.MinVersion.Value(),
	}, nil
}

func listenAndServe(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}
