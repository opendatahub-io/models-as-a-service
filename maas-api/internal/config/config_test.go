package config //nolint:testpackage // tests access unexported fields

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"testing"
)

const testGatewayName = "my-gateway"

// resetGlobalFlags replaces flag.CommandLine with a fresh FlagSet so that
// Load() can register its flags again without panicking.
func resetGlobalFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
}

func TestLoad_EnvironmentVariables(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name:    "GATEWAY_NAME overrides GatewayName and Name defaults to it",
			envVars: map[string]string{"GATEWAY_NAME": testGatewayName},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.GatewayName != testGatewayName {
					t.Errorf("expected GatewayName %q, got %q", testGatewayName, cfg.GatewayName)
				}
				// Name defaults to GatewayName when INSTANCE_NAME is not set
				if cfg.Name != testGatewayName {
					t.Errorf("expected Name to default to GatewayName %q, got %q", testGatewayName, cfg.Name)
				}
			},
		},
		{
			name:    "INSTANCE_NAME and GATEWAY_NAME set independently",
			envVars: map[string]string{"INSTANCE_NAME": "my-instance", "GATEWAY_NAME": testGatewayName},
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Name != "my-instance" {
					t.Errorf("expected Name 'my-instance', got %q", cfg.Name)
				}
				if cfg.GatewayName != testGatewayName {
					t.Errorf("expected GatewayName %q, got %q", testGatewayName, cfg.GatewayName)
				}
			},
		},
	}

	// All env vars that Load() reads, to be cleared before each subtest.
	allEnvVars := []string{
		"DEBUG_MODE", "GATEWAY_NAME", "SECURE", "INSTANCE_NAME",
		"NAMESPACE", "GATEWAY_NAMESPACE", "ADDRESS",
		"PORT",
		"TLS_CERT", "TLS_KEY", "TLS_SELF_SIGNED",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetGlobalFlags()

			// Clear all config env vars first.
			for _, key := range allEnvVars {
				t.Setenv(key, "")
				os.Unsetenv(key)
			}

			// Set the test-specific env vars.
			for key, val := range tt.envVars {
				t.Setenv(key, val)
			}

			cfg := Load()
			tt.check(t, cfg)
		})
	}
}

// TestValidate covers Config.Validate:
// required fields (DBConnectionURL),
// TLS consistency (secure without certs, cert without key),
// APIKeyMaxExpirationDays bounds,
// and default address assignment.
func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		expectError string
	}{
		{
			name: "missing DBConnectionURL returns error",
			cfg: Config{
				DBConnectionURL: "",
			},
			expectError: "db connection URL is required",
		},
		{
			name: "secure without TLS returns error",
			cfg: Config{
				DBConnectionURL: "postgresql://localhost/test",
				Secure:          true,
			},
			expectError: "--secure requires either --tls-cert/--tls-key or --tls-self-signed",
		},
		{
			name: "TLS cert without key returns error",
			cfg: Config{
				DBConnectionURL: "postgresql://localhost/test",
				TLS:             TLSConfig{Cert: "/cert.pem"},
			},
			expectError: "--tls-cert and --tls-key must both be provided together",
		},
		{
			name: "valid insecure config sets default address :8080",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				Secure:                    false,
				APIKeyMaxExpirationDays:   30,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "valid secure config with self-signed sets default address :8443",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				TLS:                       TLSConfig{SelfSigned: true, MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyMaxExpirationDays:   30,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "valid secure config with certs",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				TLS:                       TLSConfig{Cert: "/cert.pem", Key: "/key.pem", MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyMaxExpirationDays:   30,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid minimum value",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				APIKeyMaxExpirationDays:   1,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid default value",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				APIKeyMaxExpirationDays:   30,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid large value",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				APIKeyMaxExpirationDays:   365,
				AccessCheckTimeoutSeconds: 15,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
		},
		{
			name: "APIKeyMaxExpirationDays zero returns error",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				APIKeyMaxExpirationDays:   0,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
			expectError: "must be at least 1",
		},
		{
			name: "APIKeyMaxExpirationDays negative returns error",
			cfg: Config{
				DBConnectionURL:           "postgresql://localhost/test",
				APIKeyMaxExpirationDays:   -1,
				MaaSSubscriptionNamespace: "models-as-a-service",
			},
			expectError: "must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear TLS_MIN_VERSION to avoid interference from host environment.
			t.Setenv("TLS_MIN_VERSION", "")
			os.Unsetenv("TLS_MIN_VERSION")

			err := tt.cfg.Validate()

			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.expectError)
				}
				if !strings.Contains(err.Error(), tt.expectError) {
					t.Errorf("expected error containing %q, got %q", tt.expectError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify default address assignment for valid configs.
			if tt.cfg.Secure {
				if tt.cfg.Address != DefaultSecureAddr {
					t.Errorf("expected Address %q for secure config, got %q", DefaultSecureAddr, tt.cfg.Address)
				}
			} else {
				if tt.cfg.Address != DefaultInsecureAddr {
					t.Errorf("expected Address %q for insecure config, got %q", DefaultInsecureAddr, tt.cfg.Address)
				}
			}
		})
	}
}

func TestHandleDeprecatedFlags(t *testing.T) {
	t.Run("deprecated port sets Address and clears Secure", func(t *testing.T) {
		cfg := &Config{
			Secure:             true,
			deprecatedHTTPPort: "9090",
		}
		cfg.handleDeprecatedFlags()

		if cfg.Secure {
			t.Error("expected Secure to be false when deprecated port is used")
		}
		if cfg.Address != ":9090" {
			t.Errorf("expected Address ':9090', got %q", cfg.Address)
		}
	})

	t.Run("deprecated port does not override existing Address", func(t *testing.T) {
		cfg := &Config{
			Address:            ":7777",
			deprecatedHTTPPort: "9090",
		}
		cfg.handleDeprecatedFlags()

		if cfg.Address != ":7777" {
			t.Errorf("expected Address ':7777' to be preserved, got %q", cfg.Address)
		}
	})

	t.Run("no deprecated port is a no-op", func(t *testing.T) {
		cfg := &Config{
			Secure:  true,
			Address: ":8443",
		}
		cfg.handleDeprecatedFlags()

		if !cfg.Secure {
			t.Error("expected Secure to remain true")
		}
		if cfg.Address != ":8443" {
			t.Errorf("expected Address ':8443', got %q", cfg.Address)
		}
	})
}
