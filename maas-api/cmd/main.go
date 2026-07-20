package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/rest"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tenant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tracing"
)

func main() {
	if err := serve(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

const (
	tlsProfileFetchMaxRetries = 3
	tlsProfileFetchTimeout    = 10 * time.Second
	tlsProfileFetchRetryDelay = 2 * time.Second
)

var (
	fetchClusterTLSSettings = tlsprofile.FetchTLSSettings
	tlsProfileRetryDelay    = tlsProfileFetchRetryDelay
)

func serve() error {
	cfg := config.Load()
	flag.Parse()

	log := logger.New(cfg.DebugMode)
	defer func() {
		if err := log.Sync(); err != nil {
			// Can't use logger if sync failed
			fmt.Fprintf(os.Stderr, "failed to sync logger: %v\n", err)
		}
	}()

	cfg.PrintDeprecationWarnings(log)

	// Create cluster config early to load database URL from secret
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metricsRegistry := prometheus.NewRegistry()

	cluster, err := config.NewClusterConfig(cfg.Namespace, cfg.MaaSSubscriptionNamespace, constant.DefaultResyncPeriod, cfg.SARCacheMaxSize, metricsRegistry, log)
	if err != nil {
		return fmt.Errorf("failed to create cluster config: %w", err)
	}

	// Load database connection URL from Kubernetes secret
	log.Info("Loading database connection URL from secret...")
	if err := cfg.LoadDatabaseURL(ctx, cluster.ClientSet); err != nil {
		return fmt.Errorf("failed to load database URL: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	if cfg.DebugMode {
		gin.SetMode(gin.DebugMode)
	}

	// Initialize OTEL tracing (noop if endpoint not configured)
	tracingShutdown, err := tracing.InitTracer(
		ctx, cfg.OTELEndpoint, cfg.OTELInsecure, cfg.OTELSampleRate,
		"maas-api", cfg.Namespace,
	)
	if err != nil {
		return fmt.Errorf("failed to initialize tracing: %w", err)
	}
	defer tracingShutdown(ctx)
	if cfg.OTELEndpoint != "" {
		log.Info("OTEL tracing enabled", "endpoint", cfg.OTELEndpoint)
	}

	// Use gin.New() instead of gin.Default() to control middleware order
	router := gin.New()

	// Recovery must be first to catch panics from subsequent middleware
	router.Use(gin.Recovery())
	router.Use(middleware.BodyLimit())
	accessLogCfg := middleware.TenantLoggerConfig{
		DefaultTenant:   cfg.TenantName,
		TenantNamespace: cfg.MaaSSubscriptionNamespace,
		GatewayName:     cfg.GatewayName,
	}

	router.Use(middleware.RequestID())
	router.Use(middleware.AccessLogger(log, accessLogCfg))
	router.Use(tracing.NewMiddleware(cfg.TenantName, cfg.MaaSSubscriptionNamespace, cfg.GatewayName, cfg.GatewayNamespace))

	// Add metrics middleware
	metricsRecorder, err := metrics.NewPrometheusRecorder(metricsRegistry)
	if err != nil {
		return fmt.Errorf("failed to create metrics recorder: %w", err)
	}
	router.Use(metrics.NewMiddleware(metricsRecorder, cfg.TenantName))

	// Start metrics server
	metricsSrv, err := metrics.NewMetricsServer(cfg.MetricsAddress(), metricsRegistry)
	if err != nil {
		return fmt.Errorf("failed to create metrics server: %w", err)
	}
	metricsErr := make(chan error, 1)
	go func() {
		log.Info("Metrics server starting", "address", cfg.MetricsAddress())
		metricsErr <- metricsSrv.ListenAndServe()
	}()

	if cfg.DebugMode {
		log.Warn("Debug CORS policy active: allowing localhost origins only")
		router.Use(cors.New(debugCORSConfig()))
	}

	router.OPTIONS("/*path", func(c *gin.Context) { c.Status(204) })

	store, err := initStore(ctx, log, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize token store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("Failed to close token store", "error", err)
		}
	}()

	if err = registerHandlers(ctx, log, router, cfg, cluster, store, metricsRecorder); err != nil {
		return fmt.Errorf("failed to register handlers: %w", err)
	}

	profileMinVersion, profileCipherSuites, tlsErr := setupTLSProfile(ctx, log, cfg, cluster, cancel)
	if tlsErr != nil {
		return fmt.Errorf("failed to set up TLS profile: %w", tlsErr)
	}

	srv, err := newServer(cfg, router, profileMinVersion, profileCipherSuites)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Channel to capture server startup errors from the goroutine
	serverErr := make(chan error, 1)
	go func() {
		log.Info("Server starting", "address", cfg.Address, "secure", cfg.Secure)
		serverErr <- listenAndServe(srv)
		close(serverErr)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server failed to start: %w", err)
		}
	case err := <-metricsErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("metrics server failed: %w", err)
		}
	case <-ctx.Done():
		log.Info("Context cancelled (TLS profile change or shutdown), shutting down server...")
	case <-quit:
		log.Info("Shutdown signal received, shutting down server...")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server forced to shutdown: %w", err)
	}

	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("Metrics server forced to shutdown", "error", err)
	}

	log.Info("Server exited gracefully")
	return nil
}

// initStore creates the PostgreSQL store for API key management.
// DBConnectionURL is validated in cfg.Validate() before this is called.
func initStore(ctx context.Context, log *logger.Logger, cfg *config.Config) (api_keys.MetadataStore, error) { //nolint:ireturn // Returns MetadataStore interface by design.
	log.Info("Connecting to PostgreSQL database...", "tenant", cfg.TenantName)
	return api_keys.NewPostgresStoreFromURL(ctx, log, cfg.DBConnectionURL, cfg.TenantName)
}

func registerHandlers(
	ctx context.Context,
	log *logger.Logger,
	router *gin.Engine,
	cfg *config.Config,
	cluster *config.ClusterConfig,
	store api_keys.MetadataStore,
	metricsRecorder *metrics.PrometheusRecorder,
) error {
	router.GET("/health", handlers.NewHealthHandler().HealthCheck)

	log.Info("Starting informers and waiting for cache sync...")
	if !cluster.StartAndWaitForSync(ctx.Done()) {
		return errors.New("failed to sync informer caches")
	}
	log.Info("Informer caches synced successfully")

	v1Routes := router.Group("/v1")

	authPolicyChecker := authpolicy.NewChecker(log, cluster.MaaSAuthPolicyLister)
	subscriptionSelector := subscription.NewSelector(log, cluster.MaaSSubscriptionLister, cluster.MaaSModelRefLister, authPolicyChecker)

	resolveCtx, resolveCancel := context.WithTimeout(ctx, time.Duration(cfg.AccessCheckTimeoutSeconds)*time.Second)
	gatewayInternalHost, err := config.ResolveGatewayInternalHost(resolveCtx, cluster.ClientSet, cfg.GatewayName, cfg.GatewayNamespace)
	resolveCancel()
	if err != nil {
		return fmt.Errorf("failed to resolve gateway internal address: %w", err)
	}
	if gatewayInternalHost == "" {
		return fmt.Errorf("gateway service not found for %s/%s: model access probes require a resolvable gateway internal host",
			cfg.GatewayNamespace, cfg.GatewayName)
	}
	log.Info("Resolved gateway internal host for access probes", "host", gatewayInternalHost)

	modelManager, err := models.NewManager(log, cfg.AccessCheckTimeoutSeconds, gatewayInternalHost, cfg.DiscoveryEnableHTTP2)
	if err != nil {
		log.Fatal("Failed to create model manager", "error", err)
	}

	tokenHandler := token.NewHandler(log, cfg.TenantName)
	modelsHandler := handlers.NewModelsHandler(log, modelManager, subscriptionSelector, cluster.MaaSModelRefLister)
	subscriptionHandler := subscription.NewHandler(log, subscriptionSelector)

	apiKeyService := api_keys.NewServiceWithLogger(store, cfg, subscriptionSelector, log)
	apiKeyService.StartDebounceCleanup(ctx)
	apiKeyHandler := api_keys.NewHandler(log, apiKeyService, cluster.AdminChecker, metricsRecorder)

	tenantLogCfg := middleware.TenantLoggerConfig{
		DefaultTenant:   cfg.TenantName,
		TenantNamespace: cfg.MaaSSubscriptionNamespace,
		GatewayName:     cfg.GatewayName,
	}
	// Optional-auth middleware lets handlers return graceful responses (e.g.
	// empty lists) when no LLMInferenceService is deployed and Authorino has
	// no auth policy to inject identity headers.
	optionalAuthMiddleware := []gin.HandlerFunc{tokenHandler.ExtractUserInfoOptional(), middleware.TenantLogger(log, tenantLogCfg)}
	v1Routes.GET("/models", append(optionalAuthMiddleware, modelsHandler.ListLLMs)...)

	// Subscription listing routes use optional auth so they can return an empty
	// list when no LLMInferenceService is deployed (same rationale as /v1/models).
	v1Routes.GET("/subscriptions", append(optionalAuthMiddleware, subscriptionHandler.ListSubscriptions)...)
	v1Routes.GET("/model/:model-id/subscriptions", append(optionalAuthMiddleware, subscriptionHandler.ListSubscriptionsForModel)...)

	// API Key routes use strict auth for mutating operations.
	// Only the search/listing endpoint uses optional auth so it can return
	// an empty result when no LLMInferenceService is deployed (same rationale
	// as /v1/models and /v1/subscriptions).
	strictAuthMiddleware := []gin.HandlerFunc{tokenHandler.ExtractUserInfo(), middleware.TenantLogger(log, tenantLogCfg)}
	apiKeyRoutes := v1Routes.Group("/api-keys", strictAuthMiddleware...)
	apiKeyRoutes.GET("/config", apiKeyHandler.GetAPIKeyConfig)         // Get API key limits
	apiKeyRoutes.POST("", apiKeyHandler.CreateAPIKey)                  // Create hash-based key
	apiKeyRoutes.POST("/bulk-revoke", apiKeyHandler.BulkRevokeAPIKeys) // Bulk revoke keys
	apiKeyRoutes.GET("/:id", apiKeyHandler.GetAPIKey)                  // Get specific key
	apiKeyRoutes.DELETE("/:id", apiKeyHandler.RevokeAPIKey)            // Revoke specific key

	// API key search uses optional auth — returns empty list when no auth context
	v1Routes.POST("/api-keys/search", append(optionalAuthMiddleware, apiKeyHandler.SearchAPIKeys)...)

	// Tenant/Gateway discovery route - authenticated via TokenReview + SubjectAccessReview (system:authenticated)
	tenantHandler := tenant.NewHandler(log, cluster.DynamicClient, cfg.TenantName, cfg.GatewayName, cfg.GatewayNamespace)
	v1Routes.GET("/tenants",
		auth.TenantAuthMiddleware(log, cluster.ClientSet), //nolint:contextcheck // gin middleware uses c.Request.Context()
		tenantHandler.GetTenantInfo)

	// Internal routes (no auth required - called by Authorino / CronJob)
	internalRoutes := router.Group("/internal/v1")
	internalRoutes.POST("/api-keys/validate", apiKeyHandler.ValidateAPIKeyHandler)
	internalRoutes.POST("/api-keys/cleanup", apiKeyHandler.CleanupExpiredEphemeralKeys)
	internalRoutes.DELETE("/tenants/:tenant/api-keys", apiKeyHandler.RevokeTenantAPIKeys)
	internalRoutes.POST("/subscriptions/select", subscriptionHandler.SelectSubscription)

	return nil
}

// isLocalhostOrigin reports whether the origin is an http://localhost or
// http://127.0.0.1 address, used by the debug-mode CORS policy to restrict
// cross-origin access to local development only.
// Only plain HTTP is accepted — local dev servers do not use HTTPS.
// (CWE-942 / FIND-Debug-CORS.)
func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func debugCORSConfig() cors.Config {
	return cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "Accept"},
		ExposeHeaders:    []string{"Content-Type"},
		AllowOriginFunc:  isLocalhostOrigin,
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}
}

// setupTLSProfile fetches the OpenShift cluster TLS security profile and starts
// a watcher that cancels the context on profile changes. Returns the profile's
// minVersion and cipherSuites for use in buildTLSConfig. When HTTPS is disabled,
// returns zero values so flag-based defaults apply.
//
// Fetch and watcher errors are logged and the server continues with the
// Intermediate profile defaults so transient config API issues do not block
// startup.
func setupTLSProfile(ctx context.Context, log *logger.Logger, cfg *config.Config, cluster *config.ClusterConfig, cancel context.CancelFunc) (uint16, []uint16, error) {
	restConfig := cluster.RESTConfig()
	if !cfg.Secure || restConfig == nil {
		return 0, nil, nil
	}

	settings, watchSettings, fetchErr := fetchTLSSettingsWithRetry(ctx, log, restConfig)
	if fetchErr != nil {
		return 0, nil, fetchErr
	}
	profile := settings.AppliedProfile()

	log.Info("Using cluster TLS security profile",
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
			log.Info("TLS profile watcher could not be created; continuing with current TLS profile",
				"error", watchErr)
		} else {
			go func() {
				if err := watcher.Start(ctx.Done()); err != nil {
					log.Info("TLS profile watcher stopped before syncing; continuing with current TLS profile",
						"error", err)
				}
			}()
		}
	}

	return profileMinVersion, profileCipherSuites, nil
}

// fetchTLSSettingsWithRetry attempts to fetch the OpenShift TLS security profile
// and adherence policy.
// If the config.openshift.io API doesn't exist (non-OpenShift), returns
// watchSettings=false with nil error. For transient errors on OpenShift, retries
// a few times before logging and returning the default Intermediate profile with
// watchSettings=true, allowing the watcher to self-heal when the API recovers.
func fetchTLSSettingsWithRetry(ctx context.Context, log *logger.Logger, restConfig *rest.Config) (tlsprofile.Settings, bool, error) {
	var lastErr error
	for attempt := range tlsProfileFetchMaxRetries {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, tlsProfileFetchTimeout)
		settings, err := fetchClusterTLSSettings(fetchCtx, restConfig)
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
			log.Info("Transient error fetching cluster TLS profile, retrying",
				"error", err, "attempt", attempt+1, "maxRetries", tlsProfileFetchMaxRetries)
			select {
			case <-ctx.Done():
				return tlsprofile.DefaultSettings(), false, ctx.Err()
			case <-time.After(tlsProfileRetryDelay):
			}
		}
	}

	log.Info("Failed to fetch cluster TLS profile after retries, using default Intermediate TLS profile",
		"error", lastErr)
	return tlsprofile.DefaultSettings(), true, nil
}
