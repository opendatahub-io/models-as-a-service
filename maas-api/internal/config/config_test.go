package config

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

// resetGlobalFlags replaces flag.CommandLine with a fresh FlagSet so that
// Load() can register its flags again without panicking.
func resetGlobalFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
}

func TestLoad_DefaultValues(t *testing.T) {
	resetGlobalFlags()

	// Unset all env vars that Load() reads to ensure clean defaults.
	for _, key := range []string{
		"DEBUG_MODE", "GATEWAY_NAME", "SECURE", "INSTANCE_NAME", "NAMESPACE", "GATEWAY_NAMESPACE", "ADDRESS",
		"API_KEY_EXPIRATION_POLICY", "PORT", "TLS_CERT", "TLS_KEY", "TLS_SELF_SIGNED",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	cfg := Load()

	tests := []struct {
		name     string
		got      any
		expected any
	}{
		{"Name defaults to gateway name", cfg.Name, constant.DefaultGatewayName},
		{"Namespace", cfg.Namespace, constant.DefaultNamespace},
		{"GatewayName", cfg.GatewayName, constant.DefaultGatewayName},
		{"GatewayNamespace", cfg.GatewayNamespace, constant.DefaultGatewayNamespace},
		{"Address is empty", cfg.Address, ""},
		{"Secure is false", cfg.Secure, false},
		{"DebugMode is false", cfg.DebugMode, false},
		{"DBConnectionURL is empty", cfg.DBConnectionURL, ""},
		{"APIKeyExpirationPolicy", cfg.APIKeyExpirationPolicy, "optional"},
		{"deprecatedHTTPPort is empty", cfg.deprecatedHTTPPort, ""},
		{"TLS.Cert is empty", cfg.TLS.Cert, ""},
		{"TLS.Key is empty", cfg.TLS.Key, ""},
		{"TLS.SelfSigned is false", cfg.TLS.SelfSigned, false},
		{"TLS.MinVersion is TLS 1.2", cfg.TLS.MinVersion, TLSVersion(tls.VersionTLS12)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, tt.got)
			}
		})
	}
}

func TestLoad_EnvironmentVariables(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name:    "INSTANCE_NAME overrides Name",
			envVars: map[string]string{"INSTANCE_NAME": "my-instance"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Name != "my-instance" {
					t.Errorf("expected Name 'my-instance', got %q", cfg.Name)
				}
			},
		},
		{
			name:    "NAMESPACE overrides Namespace",
			envVars: map[string]string{"NAMESPACE": "custom-ns"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Namespace != "custom-ns" {
					t.Errorf("expected Namespace 'custom-ns', got %q", cfg.Namespace)
				}
			},
		},
		{
			name:    "GATEWAY_NAME overrides GatewayName and Name defaults to it",
			envVars: map[string]string{"GATEWAY_NAME": "my-gateway"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.GatewayName != "my-gateway" {
					t.Errorf("expected GatewayName 'my-gateway', got %q", cfg.GatewayName)
				}
				// Name defaults to GatewayName when INSTANCE_NAME is not set
				if cfg.Name != "my-gateway" {
					t.Errorf("expected Name to default to GatewayName 'my-gateway', got %q", cfg.Name)
				}
			},
		},
		{
			name:    "INSTANCE_NAME and GATEWAY_NAME set independently",
			envVars: map[string]string{"INSTANCE_NAME": "my-instance", "GATEWAY_NAME": "my-gateway"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Name != "my-instance" {
					t.Errorf("expected Name 'my-instance', got %q", cfg.Name)
				}
				if cfg.GatewayName != "my-gateway" {
					t.Errorf("expected GatewayName 'my-gateway', got %q", cfg.GatewayName)
				}
			},
		},
		{
			name:    "GATEWAY_NAMESPACE overrides GatewayNamespace",
			envVars: map[string]string{"GATEWAY_NAMESPACE": "gw-ns"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.GatewayNamespace != "gw-ns" {
					t.Errorf("expected GatewayNamespace 'gw-ns', got %q", cfg.GatewayNamespace)
				}
			},
		},
		{
			name:    "ADDRESS overrides Address",
			envVars: map[string]string{"ADDRESS": ":9999"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Address != ":9999" {
					t.Errorf("expected Address ':9999', got %q", cfg.Address)
				}
			},
		},
		{
			name:    "SECURE=true sets Secure",
			envVars: map[string]string{"SECURE": "true"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.Secure {
					t.Error("expected Secure to be true")
				}
			},
		},
		{
			name:    "DEBUG_MODE=true sets DebugMode",
			envVars: map[string]string{"DEBUG_MODE": "true"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.DebugMode {
					t.Error("expected DebugMode to be true")
				}
			},
		},
		{
			name:    "API_KEY_EXPIRATION_POLICY=required",
			envVars: map[string]string{"API_KEY_EXPIRATION_POLICY": "required"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.APIKeyExpirationPolicy != "required" {
					t.Errorf("expected APIKeyExpirationPolicy 'required', got %q", cfg.APIKeyExpirationPolicy)
				}
			},
		},
		{
			name:    "PORT sets deprecatedHTTPPort",
			envVars: map[string]string{"PORT": "9090"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.deprecatedHTTPPort != "9090" {
					t.Errorf("expected deprecatedHTTPPort '9090', got %q", cfg.deprecatedHTTPPort)
				}
			},
		},
		{
			name:    "TLS_CERT and TLS_KEY set TLS config",
			envVars: map[string]string{"TLS_CERT": "/path/to/cert.pem", "TLS_KEY": "/path/to/key.pem"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.TLS.Cert != "/path/to/cert.pem" {
					t.Errorf("expected TLS.Cert '/path/to/cert.pem', got %q", cfg.TLS.Cert)
				}
				if cfg.TLS.Key != "/path/to/key.pem" {
					t.Errorf("expected TLS.Key '/path/to/key.pem', got %q", cfg.TLS.Key)
				}
			},
		},
		{
			name:    "TLS_SELF_SIGNED=true sets TLS.SelfSigned",
			envVars: map[string]string{"TLS_SELF_SIGNED": "true"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.TLS.SelfSigned {
					t.Error("expected TLS.SelfSigned to be true")
				}
			},
		},
	}

	// All env vars that Load() reads, to be cleared before each subtest.
	allEnvVars := []string{
		"DEBUG_MODE", "GATEWAY_NAME", "SECURE", "INSTANCE_NAME",
		"NAMESPACE", "GATEWAY_NAMESPACE", "ADDRESS",
		"API_KEY_EXPIRATION_POLICY", "PORT",
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
// APIKeyExpirationPolicy values,
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
				DBConnectionURL:        "",
				APIKeyExpirationPolicy: "optional",
			},
			expectError: "db connection URL is required",
		},
		{
			name: "secure without TLS returns error",
			cfg: Config{
				DBConnectionURL:        "postgresql://localhost/test",
				Secure:                 true,
				APIKeyExpirationPolicy: "optional",
			},
			expectError: "--secure requires either --tls-cert/--tls-key or --tls-self-signed",
		},
		{
			name: "TLS cert without key returns error",
			cfg: Config{
				DBConnectionURL:        "postgresql://localhost/test",
				TLS:                    TLSConfig{Cert: "/cert.pem"},
				APIKeyExpirationPolicy: "optional",
			},
			expectError: "--tls-cert and --tls-key must both be provided together",
		},
		{
			name: "invalid APIKeyExpirationPolicy returns error",
			cfg: Config{
				DBConnectionURL:        "postgresql://localhost/test",
				APIKeyExpirationPolicy: "invalid",
			},
			expectError: "API_KEY_EXPIRATION_POLICY must be 'optional' or 'required'",
		},
		{
			name: "valid insecure config sets default address :8080",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				Secure:                  false,
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "valid secure config with self-signed sets default address :8443",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				TLS:                     TLSConfig{SelfSigned: true, MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "valid secure config with certs",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				TLS:                     TLSConfig{Cert: "/cert.pem", Key: "/key.pem", MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid minimum value",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 1,
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid default value",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "APIKeyMaxExpirationDays valid large value",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 365,
			},
		},
		{
			name: "APIKeyMaxExpirationDays zero returns error",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: 0,
			},
			expectError: "must be at least 1",
		},
		{
			name: "APIKeyMaxExpirationDays negative returns error",
			cfg: Config{
				DBConnectionURL:         "postgresql://localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: -1,
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
