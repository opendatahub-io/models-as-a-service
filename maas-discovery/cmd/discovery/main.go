package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cert"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/handler"
)

const shutdownTimeout = 15 * time.Second

func main() {
	addr := flag.String("addr", ":8443", "listen address")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate file")
	tlsKey := flag.String("tls-key", "", "path to TLS key file")
	selfSigned := flag.Bool("self-signed", false, "generate a self-signed certificate for development")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	tlsConfig, err := buildTLSConfig(*tlsCert, *tlsKey, *selfSigned)
	if err != nil {
		log.Error("failed to configure TLS", "error", err)
		os.Exit(1)
	}

	tc := cache.NewStub()
	h := handler.New(tc)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	h.RegisterRoutes(engine)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           engine,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting discovery service", "addr", *addr)
		if serr := srv.ListenAndServeTLS("", ""); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		log.Error("server error", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "error", err)
	}
	log.Info("server stopped")
}

func buildTLSConfig(certFile, keyFile string, selfSigned bool) (*tls.Config, error) {
	var tlsCert tls.Certificate
	var err error

	switch {
	case certFile != "" && keyFile != "":
		tlsCert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
	case selfSigned:
		tlsCert, err = cert.Generate("maas-discovery")
		if err != nil {
			return nil, fmt.Errorf("generating self-signed certificate: %w", err)
		}
	default:
		return nil, errors.New("TLS is required: provide --tls-cert and --tls-key, or use --self-signed for development")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}
