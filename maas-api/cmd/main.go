package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration validation failed: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	if cfg.DebugMode {
		gin.SetMode(gin.DebugMode)
	}

	router := gin.Default()
	if cfg.DebugMode {
		router.Use(cors.New(cors.Config{
			AllowMethods:  []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowHeaders:  []string{"Authorization", "Content-Type", "Accept"},
			ExposeHeaders: []string{"Content-Type"},
			AllowOriginFunc: func(origin string) bool {
				return true
			},
			AllowCredentials: true,
			MaxAge:           12 * time.Hour,
		}))
	}

	router.OPTIONS("/*path", func(c *gin.Context) { c.Status(204) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := initStore(ctx, log, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize token store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("Failed to close token store", "error", err)
		}
	}()

	if err := registerHandlers(ctx, log, router, cfg, store); err != nil {
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

// initStore creates the store based on the configured storage mode.
//
// Storage modes:
//   - in-memory (default): Ephemeral storage, data lost on restart
//   - disk: Persistent local storage using a file (single replica only)
//   - external: External database (PostgreSQL), supports multiple replicas
//
//nolint:ireturn // Returns MetadataStore interface by design for pluggable storage backends.
func initStore(ctx context.Context, log *logger.Logger, cfg *config.Config) (api_keys.MetadataStore, error) {
	switch cfg.StorageMode {
	case config.StorageModeInMemory, "":
		log.Info("Using in-memory storage (data will be lost on restart). " +
			"For persistent storage, use --storage=disk or --storage=external")
		return api_keys.NewSQLiteStore(ctx, log, ":memory:")

	case config.StorageModeDisk:
		dataPath := strings.TrimSpace(cfg.DataPath)
		if dataPath == "" {
			dataPath = config.DefaultDataPath
		}
		log.Info("Using persistent disk storage", "path", dataPath)
		return api_keys.NewSQLiteStore(ctx, log, dataPath)

	case config.StorageModeExternal:
		dbURL := strings.TrimSpace(cfg.DBConnectionURL)
		if dbURL == "" {
			return nil, errors.New("--db-connection-url is required when using --storage=external")
		}
		log.Info("Connecting to external database...")
		return api_keys.NewExternalStore(ctx, log, dbURL)

	default:
		return nil, fmt.Errorf("unknown storage mode: %q (valid modes: in-memory, disk, external)", cfg.StorageMode)
	}
}

func registerHandlers(ctx context.Context, log *logger.Logger, router *gin.Engine, cfg *config.Config, store api_keys.MetadataStore) error {
	router.GET("/health", handlers.NewHealthHandler().HealthCheck)

	cluster, err := config.NewClusterConfig(cfg.Namespace, constant.DefaultResyncPeriod)
	if err != nil {
		return fmt.Errorf("failed to create cluster config: %w", err)
	}

	if !cluster.StartAndWaitForSync(ctx.Done()) {
		return errors.New("failed to sync informer caches")
	}

	v1Routes := router.Group("/v1")

	tierMapper := tier.NewMapper(log, cluster.ConfigMapLister, cfg.Name, cfg.Namespace)
	v1Routes.POST("/tiers/lookup", tier.NewHandler(tierMapper).TierLookup)

	modelManager, err := models.NewManager(
		log,
		cluster.LLMInferenceServiceLister,
		cluster.HTTPRouteLister,
		models.GatewayRef{Name: cfg.GatewayName, Namespace: cfg.GatewayNamespace},
	)
	if err != nil {
		log.Fatal("Failed to create model manager", "error", err)
	}

	tokenManager := token.NewManager(
		log,
		cfg.Name,
		tierMapper,
		cluster.ClientSet,
		cluster.NamespaceLister,
		cluster.ServiceAccountLister,
	)
	tokenHandler := token.NewHandler(log, cfg.Name, tokenManager)

	modelsHandler := handlers.NewModelsHandler(log, modelManager, tokenManager)

	apiKeyService := api_keys.NewService(tokenManager, store)
	apiKeyHandler := api_keys.NewHandler(log, apiKeyService)

	v1Routes.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	tokenRoutes := v1Routes.Group("/tokens", tokenHandler.ExtractUserInfo())
	tokenRoutes.POST("", tokenHandler.IssueToken)
	tokenRoutes.DELETE("", apiKeyHandler.RevokeAllTokens)

	apiKeyRoutes := v1Routes.Group("/api-keys", tokenHandler.ExtractUserInfo())
	apiKeyRoutes.POST("", apiKeyHandler.CreateAPIKey)
	apiKeyRoutes.GET("", apiKeyHandler.ListAPIKeys)
	apiKeyRoutes.GET("/:id", apiKeyHandler.GetAPIKey)

	return nil
}
