package config

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/env"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

const (
	DefaultSecureAddr   = ":8443"
	DefaultInsecureAddr = ":8080"
)

type Config struct {
	Name      string
	Namespace string

	GatewayName      string
	GatewayNamespace string

	MaaSSubscriptionNamespace string

	// TenantName is the tenant identifier for this maas-api instance.
	// Set to "models-as-a-service" for default tenant, or AITenant name (e.g., "redteam") for other tenants.
	// Used to filter database queries to enforce tenant isolation.
	TenantName string

	// Server configuration
	Address string // Listen address for HTTPS (host:port)
	Secure  bool   // Use HTTPS
	TLS     TLSConfig

	DebugMode bool

	// DBConnectionURL is the PostgreSQL connection URL.
	// Format: postgresql://user:password@host:port/database
	DBConnectionURL string

	// APIKeyMaxExpirationDays is the maximum allowed expiration in days for API keys.
	// Users cannot create API keys with expiration longer than this value.
	// Default: 30 days. Minimum: 1 day.
	APIKeyMaxExpirationDays int

	// AccessCheckTimeoutSeconds bounds the total duration of model access validation.
	// This limits the staleness window between when access is checked and when the
	// response reaches the client. Models whose probes don't complete within this
	// window are excluded (fail-closed). Default: 15 seconds. Minimum: 1 second.
	AccessCheckTimeoutSeconds int

	// SARCacheMaxSize is the maximum number of entries in the SAR admin-check cache.
	// Bounds memory usage under high-cardinality user traffic. Default: 8192.
	SARCacheMaxSize int

	// LastUsedDebounceSecs is the minimum number of seconds between consecutive
	// last_used_at writes to Postgres for the same API key. When many requests
	// share a single key (e.g. load tests), only one UPDATE is issued per window
	// instead of one per request, preventing row-lock contention.
	// Set to 0 to disable debouncing (every validation writes to DB). Default: 60.
	LastUsedDebounceSecs int

	MetricsPort int

	// OTELEndpoint is the OTLP gRPC endpoint for trace export (e.g., "localhost:4317").
	// Tracing is disabled when empty.
	OTELEndpoint string

	// OTELInsecure disables TLS for the OTLP exporter connection.
	OTELInsecure bool

	// OTELSampleRate controls the fraction of traces sampled (0.0 to 1.0).
	// Default: 1.0 (sample everything). Set lower in production for high-volume APIs.
	OTELSampleRate float64

	// Deprecated flag (backward compatibility with pre-TLS version)
	deprecatedHTTPPort string
}

// Load loads configuration from environment variables.
func Load() *Config {
	debugMode, _ := env.GetBool("DEBUG_MODE", false)
	gatewayName := env.GetString("GATEWAY_NAME", constant.DefaultGatewayName)
	secure, _ := env.GetBool("SECURE", false)
	maxExpirationDays, _ := env.GetInt("API_KEY_MAX_EXPIRATION_DAYS", constant.DefaultAPIKeyMaxExpirationDays)
	accessCheckTimeoutSeconds, _ := env.GetInt("ACCESS_CHECK_TIMEOUT_SECONDS", 15)
	sarCacheMaxSize, _ := env.GetInt("SAR_CACHE_MAX_SIZE", constant.DefaultSARCacheMaxSize)
	lastUsedDebounceSecs, _ := env.GetInt("LAST_USED_DEBOUNCE_SECS", 60)
	metricsPort, _ := env.GetInt("METRICS_PORT", constant.DefaultMetricsPort)
	otelInsecure, _ := env.GetBool("OTEL_EXPORTER_OTLP_INSECURE", false)
	otelSampleRate := 1.0
	if rateStr := env.GetString("OTEL_TRACES_SAMPLE_RATE", ""); rateStr != "" {
		if parsed, err := strconv.ParseFloat(rateStr, 64); err == nil && parsed >= 0 && parsed <= 1 {
			otelSampleRate = parsed
		}
	}

	tenantName := strings.TrimSpace(env.GetString("TENANT_NAME", "models-as-a-service"))
	if tenantName == "" {
		panic("TENANT_NAME environment variable must be non-empty (tenant isolation required)")
	}

	c := &Config{
		Name:                      env.GetString("INSTANCE_NAME", gatewayName),
		Namespace:                 env.GetString("NAMESPACE", constant.DefaultNamespace),
		GatewayName:               gatewayName,
		GatewayNamespace:          env.GetString("GATEWAY_NAMESPACE", constant.DefaultGatewayNamespace),
		MaaSSubscriptionNamespace: env.GetString("MAAS_SUBSCRIPTION_NAMESPACE", constant.DefaultMaaSSubscriptionNamespace),
		TenantName:                tenantName,
		Address:                   env.GetString("ADDRESS", ""),
		Secure:                    secure,
		TLS:                       loadTLSConfig(),
		DebugMode:                 debugMode,
		DBConnectionURL:           "", // Loaded from K8s secret via LoadDatabaseURL()
		APIKeyMaxExpirationDays:   maxExpirationDays,
		AccessCheckTimeoutSeconds: accessCheckTimeoutSeconds,
		SARCacheMaxSize:           sarCacheMaxSize,
		LastUsedDebounceSecs:      lastUsedDebounceSecs,
		MetricsPort:               metricsPort,
		OTELEndpoint:              env.GetString("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTELInsecure:              otelInsecure,
		OTELSampleRate:            otelSampleRate,
		// Deprecated env var (backward compatibility with pre-TLS version)
		deprecatedHTTPPort: env.GetString("PORT", ""),
	}

	c.bindFlags(flag.CommandLine)

	return c
}

// bindFlags will parse the given flagset and bind values to selected config options.
func (c *Config) bindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Name, "name", c.Name, "Name of the MaaS instance")
	fs.StringVar(&c.Namespace, "namespace", c.Namespace, "Namespace of the MaaS instance")
	fs.StringVar(&c.GatewayName, "gateway-name", c.GatewayName, "Name of the Gateway that has MaaS capabilities")
	fs.StringVar(&c.GatewayNamespace, "gateway-namespace", c.GatewayNamespace, "Namespace where MaaS-enabled Gateway is deployed")
	fs.StringVar(&c.MaaSSubscriptionNamespace, "maas-subscription-namespace", c.MaaSSubscriptionNamespace, "Namespace where MaaSSubscription CRs are located")

	fs.StringVar(&c.Address, "address", c.Address, "HTTPS listen address (default :8443)")
	fs.BoolVar(&c.Secure, "secure", c.Secure, "Use HTTPS (default: false)")
	c.TLS.bindFlags(fs)

	// Deprecated flag (backward compatibility with pre-TLS version)
	fs.StringVar(&c.deprecatedHTTPPort, "port", c.deprecatedHTTPPort, "DEPRECATED: use --address with --secure=false")

	fs.BoolVar(&c.DebugMode, "debug", c.DebugMode, "Enable debug mode")
	// Note: DBConnectionURL is loaded from K8s secret 'maas-db-config', not from CLI flag
}

// Validate validates the configuration after flags have been parsed.
// It returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	// Handle backward compatibility for deprecated flags
	c.handleDeprecatedFlags()

	// Validate required fields
	if c.DBConnectionURL == "" {
		return errors.New("db connection URL is required (loaded from K8s secret 'maas-db-config')")
	}

	if err := c.TLS.validate(); err != nil {
		return err
	}

	if c.TLS.Enabled() {
		c.Secure = true
	}

	if c.Secure && !c.TLS.Enabled() {
		return errors.New("--secure requires either --tls-cert/--tls-key or --tls-self-signed")
	}

	// Set default address based on secure mode
	if c.Address == "" {
		if c.Secure {
			c.Address = DefaultSecureAddr
		} else {
			c.Address = DefaultInsecureAddr
		}
	}

	if strings.TrimSpace(c.MaaSSubscriptionNamespace) == "" {
		return errors.New("MAAS_SUBSCRIPTION_NAMESPACE must be non-empty")
	}
	if errs := validation.IsDNS1123Label(c.MaaSSubscriptionNamespace); len(errs) > 0 {
		return fmt.Errorf("MAAS_SUBSCRIPTION_NAMESPACE %q is invalid: %v", c.MaaSSubscriptionNamespace, errs)
	}

	// Validate TenantName is non-empty and non-whitespace to ensure tenant isolation
	if strings.TrimSpace(c.TenantName) == "" {
		return errors.New("TENANT_NAME must be non-empty and non-whitespace to ensure tenant isolation")
	}

	// Validate API key max expiration days
	if c.APIKeyMaxExpirationDays < 1 {
		return errors.New("API_KEY_MAX_EXPIRATION_DAYS must be at least 1")
	}

	if c.AccessCheckTimeoutSeconds < 1 {
		return errors.New("ACCESS_CHECK_TIMEOUT_SECONDS must be at least 1")
	}

	if c.LastUsedDebounceSecs < 0 {
		return errors.New("LAST_USED_DEBOUNCE_SECS must be greater than or equal to 0")
	}

	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return errors.New("METRICS_PORT must be between 1 and 65535")
	}

	return nil
}

// handleDeprecatedFlags maps deprecated flags to new configuration.
func (c *Config) handleDeprecatedFlags() {
	// If deprecated --port flag is used, map to new model (HTTP mode)
	if c.deprecatedHTTPPort != "" {
		c.Secure = false
		if c.Address == "" {
			c.Address = ":" + c.deprecatedHTTPPort
		}
	}
}

// PrintDeprecationWarnings prints warnings for deprecated flags to stderr.
func (c *Config) PrintDeprecationWarnings(log *logger.Logger) {
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			log.Warn("WARNING: --port is deprecated, use --address with --secure=false to serve insecure HTTP traffic")
		}
	})
}

// MetricsAddress returns the listen address for the metrics server.
func (c *Config) MetricsAddress() string {
	return fmt.Sprintf(":%d", c.MetricsPort)
}

// LoadDatabaseURL reads the database connection URL from the Kubernetes secret.
// This replaces the previous approach of reading from environment variables.
//
// Secret: maas-db-config (in the same namespace as the pod)
// Key: DB_CONNECTION_URL
//
// Returns an error if the secret is missing or the key is not found.
func (c *Config) LoadDatabaseURL(ctx context.Context, clientset *kubernetes.Clientset) error {
	const (
		//nolint:gosec // This is the documented name of the secret, not a hardcoded credential.
		secretName = "maas-db-config"
		secretKey  = "DB_CONNECTION_URL"
	)

	secret, err := clientset.CoreV1().Secrets(c.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to read secret %s/%s: %w (ensure the secret exists before starting maas-api)", c.Namespace, secretName, err)
	}

	dbURL, ok := secret.Data[secretKey]
	if !ok || len(dbURL) == 0 {
		return fmt.Errorf("key %q not found or empty in secret %s/%s", secretKey, c.Namespace, secretName)
	}

	c.DBConnectionURL = string(dbURL)
	return nil
}
