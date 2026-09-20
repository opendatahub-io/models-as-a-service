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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cert"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/handler"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/tlsprofile"
)

const (
	shutdownTimeout           = 15 * time.Second
	tlsProfileFetchMaxRetries = 3
	tlsProfileFetchTimeout    = 10 * time.Second
	tlsProfileFetchRetryDelay = 2 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8443", "listen address")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate file")
	tlsKey := flag.String("tls-key", "", "path to TLS key file")
	selfSigned := flag.Bool("self-signed", false, "generate a self-signed certificate for development")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig file (out-of-cluster only)")
	tenantNamespace := flag.String("aitenant-namespace", "ai-tenants", "namespace where AITenant CRs are created")
	gatewayNamespace := flag.String("gateway-namespace", "openshift-ingress", "namespace of Gateway resources")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	restConfig, restErr := getRestConfig(*kubeconfig)
	if restErr != nil {
		if *kubeconfig != "" {
			return fmt.Errorf("loading kubeconfig %s: %w", *kubeconfig, restErr)
		}
		log.Warn("no kubeconfig available, using stub cache and default TLS profile for development", "error", restErr)
	}

	profileMinVersion, profileCipherSuites, err := setupTLSProfile(ctx, log, restConfig, cancel)
	if err != nil {
		return fmt.Errorf("setting up TLS profile: %w", err)
	}

	tlsConfig, err := buildTLSConfig(*tlsCert, *tlsKey, *selfSigned, profileMinVersion, profileCipherSuites)
	if err != nil {
		return fmt.Errorf("configuring TLS: %w", err)
	}

	tc, err := buildCache(ctx, log, restConfig, *tenantNamespace, *gatewayNamespace)
	if err != nil {
		return fmt.Errorf("creating cache: %w", err)
	}

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
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		log.Info("context cancelled, shutting down")
	}

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown error: %w", err)
	}
	log.Info("server stopped")
	return nil
}

//nolint:ireturn // returns Stub or InformerCache depending on environment
func buildCache(ctx context.Context, log *slog.Logger, restConfig *rest.Config, tenantNS, gatewayNS string) (cache.TenantCache, error) {
	if restConfig == nil {
		return cache.NewStub(), nil
	}

	ic, err := cache.NewInformerCache(cache.InformerCacheOptions{
		RestConfig:       restConfig,
		TenantNamespace:  tenantNS,
		GatewayNamespace: gatewayNS,
		Log:              log,
	})
	if err != nil {
		return nil, fmt.Errorf("creating informer cache: %w", err)
	}

	if err := ic.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting informer cache: %w", err)
	}

	return ic, nil
}

func getRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	}
	return cfg, nil
}

func buildTLSConfig(certFile, keyFile string, selfSigned bool, profileMinVersion uint16, profileCipherSuites []uint16) (*tls.Config, error) {
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

	minVersion := profileMinVersion
	if minVersion == 0 {
		minVersion = tls.VersionTLS12
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   minVersion,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if len(profileCipherSuites) > 0 {
		cfg.CipherSuites = profileCipherSuites
	}

	return cfg, nil
}

func setupTLSProfile(ctx context.Context, log *slog.Logger, restConfig *rest.Config, cancel context.CancelFunc) (uint16, []uint16, error) {
	if restConfig == nil {
		return 0, nil, nil
	}

	settings, watchSettings, fetchErr := fetchTLSSettingsWithRetry(ctx, log, restConfig)
	if fetchErr != nil {
		return 0, nil, fetchErr
	}
	profile := settings.AppliedProfile()

	log.Info("using cluster TLS security profile",
		"configuredType", string(settings.Profile.Type),
		"appliedType", string(profile.Type),
		"minTLSVersion", profile.MinTLSVersion,
		"tlsAdherence", settings.Adherence)

	profileMinVersion, profileCipherSuites, unsupported := tlsprofile.TLSConfigFromProfile(profile)
	if len(unsupported) > 0 {
		log.Warn("TLS profile contains ciphers not supported by this Go version (ignored)",
			"unsupportedCiphers", unsupported)
	}
	if len(profileCipherSuites) == 0 && profileMinVersion < tls.VersionTLS13 {
		log.Warn("TLS profile produced no TLS 1.2 cipher suites; Go defaults will be used for TLS 1.2 negotiation")
	}

	if watchSettings {
		watcher, watchErr := tlsprofile.NewWatcher(restConfig, settings, func(oldSettings, newSettings tlsprofile.Settings) {
			log.Info("TLS security profile or adherence policy changed, initiating graceful shutdown to reload",
				"oldType", string(oldSettings.Profile.Type), "newType", string(newSettings.Profile.Type),
				"oldAdherence", oldSettings.Adherence, "newAdherence", newSettings.Adherence)
			cancel()
		})
		if watchErr != nil {
			return 0, nil, fmt.Errorf("unable to create TLS profile watcher: %w", watchErr)
		}
		if err := watcher.Start(ctx.Done()); err != nil {
			return 0, nil, fmt.Errorf("TLS profile watcher failed to sync: %w", err)
		}
	}

	return profileMinVersion, profileCipherSuites, nil
}

func fetchTLSSettingsWithRetry(ctx context.Context, log *slog.Logger, restConfig *rest.Config) (tlsprofile.Settings, bool, error) {
	var lastErr error
	for attempt := range tlsProfileFetchMaxRetries {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, tlsProfileFetchTimeout)
		settings, err := tlsprofile.FetchTLSSettings(fetchCtx, restConfig)
		fetchCancel()

		if err == nil {
			return settings, true, nil
		}

		if tlsprofile.IsAPIUnavailable(err) {
			log.Info("config.openshift.io API not available, using default Intermediate TLS profile "+
				"(expected on non-OpenShift clusters)", "error", err)
			return tlsprofile.DefaultSettings(), false, nil
		}

		lastErr = err
		if attempt < tlsProfileFetchMaxRetries-1 {
			log.Info("transient error fetching cluster TLS profile, retrying",
				"error", err, "attempt", attempt+1, "maxRetries", tlsProfileFetchMaxRetries)
			select {
			case <-ctx.Done():
				return tlsprofile.DefaultSettings(), false, ctx.Err()
			case <-time.After(tlsProfileFetchRetryDelay):
			}
		}
	}

	log.Info("failed to fetch cluster TLS profile after retries, using default Intermediate profile",
		"error", lastErr)
	return tlsprofile.DefaultSettings(), true, nil
}
