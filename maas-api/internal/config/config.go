package config

import (
	"errors"
	"flag"

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

	// Server configuration
	Address string // Listen address (host:port)
	Secure  bool   // Use HTTPS
	TLS     TLSConfig

	DebugMode bool

	// DBConnectionURL is the PostgreSQL connection URL.
	// Format: postgresql://user:password@host:port/database
	DBConnectionURL string

	// Deprecated flag (backward compatibility with pre-TLS version)
	deprecatedHTTPPort string
}

// Load loads configuration from environment variables.
func Load() *Config {
	debugMode, _ := env.GetBool("DEBUG_MODE", false)
	gatewayName := env.GetString("GATEWAY_NAME", constant.DefaultGatewayName)
	secure, _ := env.GetBool("SECURE", false)

	c := &Config{
		Name:             env.GetString("INSTANCE_NAME", gatewayName),
		Namespace:        env.GetString("NAMESPACE", constant.DefaultNamespace),
		GatewayName:      gatewayName,
		GatewayNamespace: env.GetString("GATEWAY_NAMESPACE", constant.DefaultGatewayNamespace),
		Address:          env.GetString("ADDRESS", ""),
		Secure:           secure,
		TLS:              loadTLSConfig(),
		DebugMode:        debugMode,
		DBConnectionURL:  env.GetString("DB_CONNECTION_URL", ""),
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

	fs.StringVar(&c.Address, "address", c.Address, "Listen address (default :8443 for secure, :8080 for insecure)")
	fs.BoolVar(&c.Secure, "secure", c.Secure, "Use HTTPS (default: false)")
	c.TLS.bindFlags(fs)

	// Deprecated flag (backward compatibility with pre-TLS version)
	fs.StringVar(&c.deprecatedHTTPPort, "port", c.deprecatedHTTPPort, "DEPRECATED: use --address with --secure=false")

	fs.BoolVar(&c.DebugMode, "debug", c.DebugMode, "Enable debug mode")
	fs.StringVar(&c.DBConnectionURL, "db-connection-url", c.DBConnectionURL, "PostgreSQL connection URL (required)")
}

// Validate validates the configuration after flags have been parsed.
// It returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	// Handle backward compatibility for deprecated flags
	c.handleDeprecatedFlags()

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
