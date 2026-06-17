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
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func main() {
	if err := serve(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

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

	// Use gin.New() instead of gin.Default() to control middleware order
	router := gin.New()

	// Recovery must be first to catch panics from subsequent middleware
	router.Use(gin.Recovery())
	router.Use(middleware.RequestID())
	router.Use(middleware.AccessLogger())

	// Add metrics middleware
	metricsRecorder, err := metrics.NewPrometheusRecorder(metricsRegistry)
	if err != nil {
		return fmt.Errorf("failed to create metrics recorder: %w", err)
	}
	router.Use(metrics.NewMiddleware(metricsRecorder))

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

	if err = registerHandlers(ctx, log, router, cfg, cluster, store); err != nil {
		return fmt.Errorf("failed to register handlers: %w", err)
	}

	profileMinVersion, profileCipherSuites, tlsErr := setupTLSProfile(ctx, log, cfg, cluster, cancel)
	if tlsErr != nil {
		return fmt.Errorf("refusing to start with weakened TLS policy: %w", tlsErr)
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

func registerHandlers(ctx context.Context, log *logger.Logger, router *gin.Engine, cfg *config.Config, cluster *config.ClusterConfig, store api_keys.MetadataStore) error {
	router.GET("/health", handlers.NewHealthHandler().HealthCheck)

	log.Info("Starting informers and waiting for cache sync...")
	if !cluster.StartAndWaitForSync(ctx.Done()) {
		return errors.New("failed to sync informer caches")
	}
	log.Info("Informer caches synced successfully")

	v1Routes := router.Group("/v1")

	subscriptionSelector := subscription.NewSelector(log, cluster.MaaSSubscriptionLister, cluster.MaaSModelRefLister)

	resolveCtx, resolveCancel := context.WithTimeout(ctx, time.Duration(cfg.AccessCheckTimeoutSeconds)*time.Second)
	gatewayInternalHost, err := config.ResolveGatewayInternalHost(resolveCtx, cluster.ClientSet, cfg.GatewayName, cfg.GatewayNamespace)
	resolveCancel()
	if err != nil {
		return fmt.Errorf("failed to resolve gateway internal address: %w", err)
	}
	if gatewayInternalHost == "" {
		log.Warn("No gateway service found - model access checks will be disabled",
			"gateway", cfg.GatewayName,
			"namespace", cfg.GatewayNamespace)
	} else {
		log.Info("Resolved gateway internal host for access probes", "host", gatewayInternalHost)
	}

	modelManager, err := models.NewManager(log, cfg.AccessCheckTimeoutSeconds, gatewayInternalHost)
	if err != nil {
		log.Fatal("Failed to create model manager", "error", err)
	}

	tokenHandler := token.NewHandler(log, cfg.Name)
	modelsHandler := handlers.NewModelsHandler(log, modelManager, subscriptionSelector, cluster.MaaSModelRefLister)
	subscriptionHandler := subscription.NewHandler(log, subscriptionSelector)

	apiKeyService := api_keys.NewServiceWithLogger(store, cfg, subscriptionSelector, log)
	apiKeyHandler := api_keys.NewHandler(log, apiKeyService, cluster.AdminChecker)

	v1Routes.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	// Subscription listing routes
	v1Routes.GET("/subscriptions", tokenHandler.ExtractUserInfo(), subscriptionHandler.ListSubscriptions)
	v1Routes.GET("/model/:model-id/subscriptions", tokenHandler.ExtractUserInfo(), subscriptionHandler.ListSubscriptionsForModel)

	// API Key routes - Complete CRUD for hash-based key architecture
	apiKeyRoutes := v1Routes.Group("/api-keys", tokenHandler.ExtractUserInfo())
	apiKeyRoutes.POST("", apiKeyHandler.CreateAPIKey)                  // Create hash-based key
	apiKeyRoutes.POST("/search", apiKeyHandler.SearchAPIKeys)          // Search keys with filtering, sorting, and pagination
	apiKeyRoutes.POST("/bulk-revoke", apiKeyHandler.BulkRevokeAPIKeys) // Bulk revoke keys
	apiKeyRoutes.GET("/:id", apiKeyHandler.GetAPIKey)                  // Get specific key
	apiKeyRoutes.DELETE("/:id", apiKeyHandler.RevokeAPIKey)            // Revoke specific key

	// Internal routes (no auth required - called by Authorino / CronJob)
	internalRoutes := router.Group("/internal/v1")
	internalRoutes.POST("/api-keys/validate", apiKeyHandler.ValidateAPIKeyHandler)
	internalRoutes.POST("/api-keys/cleanup", apiKeyHandler.CleanupExpiredEphemeralKeys)
	internalRoutes.POST("/subscriptions/select", subscriptionHandler.SelectSubscription)

	return nil
}

// isLocalhostOrigin reports whether the origin is a localhost address,
// used by the debug-mode CORS policy to restrict cross-origin access to
// local development only. Accepts both ported (http://localhost:3000)
// and default-port (http://localhost) forms.
func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func debugCORSConfig() cors.Config {
	return cors.Config{
		AllowMethods:    []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:    []string{"Authorization", "Content-Type", "Accept"},
		ExposeHeaders:   []string{"Content-Type"},
		AllowOriginFunc: isLocalhostOrigin,
		MaxAge:          12 * time.Hour,
	}
}

// setupTLSProfile fetches the OpenShift cluster TLS security profile and starts
// a watcher that cancels the context on profile changes. Returns the profile's
// minVersion and cipherSuites for use in buildTLSConfig. On non-OpenShift clusters
// or when HTTPS is disabled, returns zero values (flag-based defaults apply).
//
// On OpenShift, transient fetch errors are retried. If the profile cannot be
// obtained after retries, the function returns an error to prevent starting with
// a weaker default profile (fail-closed).
func setupTLSProfile(ctx context.Context, log *logger.Logger, cfg *config.Config, cluster *config.ClusterConfig, cancel context.CancelFunc) (uint16, []uint16, error) {
	restConfig := cluster.RESTConfig()
	if !cfg.Secure || restConfig == nil {
		return 0, nil, nil
	}

	profile, available, fetchErr := fetchTLSProfileWithRetry(ctx, log, restConfig)
	if fetchErr != nil {
		return 0, nil, fmt.Errorf("cluster TLS profile fetch failed after retries: %w", fetchErr)
	}
	if !available {
		return 0, nil, nil
	}

	log.Info("Fetched cluster TLS security profile",
		"type", string(profile.Type), "minTLSVersion", profile.MinTLSVersion)

	profileMinVersion, profileCipherSuites, unsupported := tlsprofile.TLSConfigFromProfile(profile)
	if len(unsupported) > 0 {
		log.Warn("TLS profile contains ciphers not supported by this Go version (ignored)",
			"unsupportedCiphers", unsupported)
	}
	if len(profileCipherSuites) == 0 && profileMinVersion < tls.VersionTLS13 {
		log.Warn("TLS profile produced no TLS 1.2 cipher suites; Go defaults will be used for TLS 1.2 negotiation")
	}

	watcher, watchErr := tlsprofile.NewWatcher(restConfig, profile, func(oldProfile, newProfile tlsprofile.ProfileSpec) {
		log.Info("TLS security profile changed, initiating graceful shutdown to reload",
			"oldType", string(oldProfile.Type), "newType", string(newProfile.Type))
		cancel()
	})
	if watchErr != nil {
		log.Warn("Could not start TLS profile watcher", "error", watchErr)
	} else {
		go func() {
			if err := watcher.Start(ctx.Done()); err != nil {
				log.Error("TLS profile watcher failed, initiating graceful shutdown to restart", "error", err)
				cancel()
			}
		}()
	}

	return profileMinVersion, profileCipherSuites, nil
}

// fetchTLSProfileWithRetry attempts to fetch the OpenShift TLS security profile.
// If the config.openshift.io API doesn't exist (non-OpenShift), returns
// available=false with nil error. For transient errors on OpenShift, retries a
// few times and returns a non-nil error if all attempts fail (fail-closed).
func fetchTLSProfileWithRetry(ctx context.Context, log *logger.Logger, restConfig *rest.Config) (tlsprofile.ProfileSpec, bool, error) {
	const maxRetries = 3

	var lastErr error
	for attempt := range maxRetries {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, 10*time.Second)
		profile, err := tlsprofile.FetchTLSProfile(fetchCtx, restConfig)
		fetchCancel()

		if err == nil {
			return profile, true, nil
		}

		if tlsprofile.IsAPIUnavailable(err) {
			log.Info("config.openshift.io API not available, using flag-based TLS defaults "+
				"(expected on non-OpenShift clusters)", "error", err)
			return tlsprofile.DefaultProfile(), false, nil
		}

		lastErr = err
		if attempt < maxRetries-1 {
			log.Info("Transient error fetching cluster TLS profile, retrying",
				"error", err, "attempt", attempt+1, "maxRetries", maxRetries)
			select {
			case <-ctx.Done():
				return tlsprofile.DefaultProfile(), false, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}

	return tlsprofile.DefaultProfile(), false, lastErr
}
