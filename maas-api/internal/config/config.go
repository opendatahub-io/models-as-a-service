package config

import (
	"flag"
	"fmt"

	"k8s.io/utils/env"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
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

const DefaultDataPath = "/data/maas-api.db"

type Config struct {
	Name      string
	Namespace string

	GatewayName      string
	GatewayNamespace string

	Port string

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
}

// Load loads configuration from environment variables.
func Load() *Config {
	debugMode, _ := env.GetBool("DEBUG_MODE", false)
	gatewayName := env.GetString("GATEWAY_NAME", constant.DefaultGatewayName)

	c := &Config{
		Name:             env.GetString("INSTANCE_NAME", gatewayName),
		Namespace:        env.GetString("NAMESPACE", constant.DefaultNamespace),
		GatewayName:      env.GetString("GATEWAY_NAME", gatewayName),
		GatewayNamespace: env.GetString("GATEWAY_NAMESPACE", constant.DefaultGatewayNamespace),
		Port:             env.GetString("PORT", "8080"),
		DebugMode:        debugMode,
		StorageMode:      StorageModeInMemory,
		DBConnectionURL:  env.GetString("DB_CONNECTION_URL", ""),
		DataPath:         env.GetString("DATA_PATH", DefaultDataPath),
	}

	// Validate STORAGE_MODE env var through Set() to ensure consistent validation
	if err := c.StorageMode.Set(env.GetString("STORAGE_MODE", "")); err != nil {
		// Log warning and fall back to default (in-memory)
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
	fs.StringVar(&c.Port, "port", c.Port, "Port to listen on")
	fs.BoolVar(&c.DebugMode, "debug", c.DebugMode, "Enable debug mode")
	fs.Var(&c.StorageMode, "storage", "Storage mode: in-memory (default), disk, or external")
	fs.StringVar(&c.DBConnectionURL, "db-connection-url", c.DBConnectionURL, "Database connection URL (required for --storage=external)")
	fs.StringVar(&c.DataPath, "data-path", c.DataPath, "Path to database file (for --storage=disk)")
}
