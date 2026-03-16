package main

import (
	"context"
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

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tier"
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

	cluster, err := config.NewClusterConfig(cfg.Namespace, constant.DefaultResyncPeriod)
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

	router := gin.Default()
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

	srv, err := newServer(cfg, router)
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
	case <-quit:
		log.Info("Shutdown signal received, shutting down server...")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server forced to shutdown: %w", err)
	}

	log.Info("Server exited gracefully")
	return nil
}

// initStore creates the PostgreSQL store for API key management.
// DBConnectionURL is validated in cfg.Validate() before this is called.
//
//nolint:ireturn // Returns MetadataStore interface by design.
func initStore(ctx context.Context, log *logger.Logger, cfg *config.Config) (api_keys.MetadataStore, error) {
	log.Info("Connecting to PostgreSQL database...")
	return api_keys.NewPostgresStoreFromURL(ctx, log, cfg.DBConnectionURL)
}

func registerHandlers(ctx context.Context, log *logger.Logger, router *gin.Engine, cfg *config.Config, cluster *config.ClusterConfig, store api_keys.MetadataStore) error {
	router.GET("/health", handlers.NewHealthHandler().HealthCheck)

	if !cluster.StartAndWaitForSync(ctx.Done()) {
		return errors.New("failed to sync informer caches")
	}

	v1Routes := router.Group("/v1")

	tierMapper := tier.NewMapper(log, cluster.ConfigMapLister, cfg.Name, cfg.Namespace)
	v1Routes.POST("/tiers/lookup", tier.NewHandler(tierMapper).TierLookup)

	subscriptionSelector := subscription.NewSelector(log, cluster.MaaSSubscriptionLister)
	v1Routes.POST("/subscriptions/select", subscription.NewHandler(log, subscriptionSelector).SelectSubscription)

	modelManager, err := models.NewManager(log)
	if err != nil {
		log.Fatal("Failed to create model manager", "error", err)
	}

	tokenHandler := token.NewHandler(log, cfg.Name)

	modelsHandler := handlers.NewModelsHandler(log, modelManager, subscriptionSelector, cluster.MaaSModelRefLister)

	apiKeyService := api_keys.NewServiceWithLogger(store, cfg, log)
	apiKeyHandler := api_keys.NewHandler(log, apiKeyService, cluster.AdminChecker)

	v1Routes.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	// API Key routes - Complete CRUD for hash-based key architecture
	apiKeyRoutes := v1Routes.Group("/api-keys", tokenHandler.ExtractUserInfo())
	apiKeyRoutes.POST("", apiKeyHandler.CreateAPIKey)                  // Create hash-based key
	apiKeyRoutes.POST("/search", apiKeyHandler.SearchAPIKeys)          // Search keys with filtering, sorting, and pagination
	apiKeyRoutes.POST("/bulk-revoke", apiKeyHandler.BulkRevokeAPIKeys) // Bulk revoke keys
	apiKeyRoutes.GET("/:id", apiKeyHandler.GetAPIKey)                  // Get specific key
	apiKeyRoutes.DELETE("/:id", apiKeyHandler.RevokeAPIKey)            // Revoke specific key

	// Internal routes for Authorino HTTP callback (no auth required - called by Authorino)
	internalRoutes := router.Group("/internal/v1")
	internalRoutes.POST("/api-keys/validate", apiKeyHandler.ValidateAPIKeyHandler)

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
