package config

import (
	"errors"
	"flag"
	"fmt"

	"k8s.io/utils/env"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// StorageMode represents the storage backend type.
type StorageMode string

const (
	StorageModeInMemory StorageMode = "in-memory"
	StorageModeDisk     StorageMode = "disk"
	StorageModeExternal StorageMode = "external"
)

// String implements flag.Value interface.
func (s *StorageMode) String() string {
	return string(*s)
}

func (s *StorageMode) Set(value string) error {
	switch StorageMode(value) {
	case StorageModeInMemory, StorageModeDisk, StorageModeExternal:
		*s = StorageMode(value)
		return nil
	case "":
		*s = StorageModeInMemory
		return nil
	default:
		return fmt.Errorf("invalid storage mode %q: valid modes are %q, %q, or %q",
			value, StorageModeInMemory, StorageModeDisk, StorageModeExternal)
	}
}

const (
	DefaultDataPath     = "/data/maas-api.db"
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

	// StorageMode specifies the storage backend type:
	//   - "in-memory" (default): Ephemeral storage, data lost on restart
	//   - "disk": Persistent local storage using a file (single replica only)
	//   - "external": External database (PostgreSQL), supports multiple replicas
	StorageMode StorageMode

	// DBConnectionURL is the database connection URL for external mode.
	DBConnectionURL string

	// DataPath is the path to the database file for disk mode.
	// Default: /data/maas-api.db
	DataPath string

	// Deprecated flag (backward compatibility with pre-TLS version)
	deprecatedHTTPPort string
}

// Load loads configuration from environment variables.
func Load() *Config {
	debugMode, _ := env.GetBool("DEBUG_MODE", false)
	gatewayName := env.GetString("GATEWAY_NAME", constant.DefaultGatewayName)
	secure, _ := env.GetBool("SECURE", true)

	c := &Config{
		Name:             env.GetString("INSTANCE_NAME", gatewayName),
		Namespace:        env.GetString("NAMESPACE", constant.DefaultNamespace),
		GatewayName:      gatewayName,
		GatewayNamespace: env.GetString("GATEWAY_NAMESPACE", constant.DefaultGatewayNamespace),
		Address:          env.GetString("ADDRESS", ""),
		Secure:           secure,
		TLS:              loadTLSConfig(),
		DebugMode:        debugMode,
		StorageMode:      StorageModeInMemory,
		DBConnectionURL:  env.GetString("DB_CONNECTION_URL", ""),
		DataPath:         env.GetString("DATA_PATH", DefaultDataPath),
		// Deprecated env var (backward compatibility with pre-TLS version)
		deprecatedHTTPPort: env.GetString("PORT", ""),
	}

	// Validate STORAGE_MODE env var through Set() to ensure consistent validation
	if err := c.StorageMode.Set(env.GetString("STORAGE_MODE", "")); err != nil {
		c.StorageMode = StorageModeInMemory
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
	fs.BoolVar(&c.Secure, "secure", c.Secure, "Use HTTPS (default: true)")
	c.TLS.bindFlags(fs)

	// Deprecated flag (backward compatibility with pre-TLS version)
	fs.StringVar(&c.deprecatedHTTPPort, "port", c.deprecatedHTTPPort, "DEPRECATED: use --address with --secure=false")

	fs.BoolVar(&c.DebugMode, "debug", c.DebugMode, "Enable debug mode")
	fs.Var(&c.StorageMode, "storage", "Storage mode: in-memory (default), disk, or external")
	fs.StringVar(&c.DBConnectionURL, "db-connection-url", c.DBConnectionURL, "Database connection URL (required for --storage=external)")
	fs.StringVar(&c.DataPath, "data-path", c.DataPath, "Path to database file (for --storage=disk)")
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
