package config

import (
	"crypto/tls"
	"flag"
	"os"
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
		"DEBUG_MODE", "GATEWAY_NAME", "SECURE", "INSTANCE_NAME",
		"NAMESPACE", "GATEWAY_NAMESPACE", "ADDRESS",
		"API_KEY_EXPIRATION_POLICY", "PORT",
		"TLS_CERT", "TLS_KEY", "TLS_SELF_SIGNED",
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

func TestBindFlags(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, cfg *Config)
	}{
		{
			name: "-name overrides Name",
			args: []string{"-name=test-instance"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Name != "test-instance" {
					t.Errorf("expected Name 'test-instance', got %q", cfg.Name)
				}
			},
		},
		{
			name: "-namespace overrides Namespace",
			args: []string{"-namespace=custom-ns"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Namespace != "custom-ns" {
					t.Errorf("expected Namespace 'custom-ns', got %q", cfg.Namespace)
				}
			},
		},
		{
			name: "-gateway-name overrides GatewayName",
			args: []string{"-gateway-name=my-gw"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.GatewayName != "my-gw" {
					t.Errorf("expected GatewayName 'my-gw', got %q", cfg.GatewayName)
				}
			},
		},
		{
			name: "-gateway-namespace overrides GatewayNamespace",
			args: []string{"-gateway-namespace=gw-ns"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.GatewayNamespace != "gw-ns" {
					t.Errorf("expected GatewayNamespace 'gw-ns', got %q", cfg.GatewayNamespace)
				}
			},
		},
		{
			name: "-address overrides Address",
			args: []string{"-address=:9999"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Address != ":9999" {
					t.Errorf("expected Address ':9999', got %q", cfg.Address)
				}
			},
		},
		{
			name: "-secure sets Secure to true",
			args: []string{"-secure"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.Secure {
					t.Error("expected Secure to be true")
				}
			},
		},
		{
			name: "-debug sets DebugMode to true",
			args: []string{"-debug"},
			check: func(t *testing.T, cfg *Config) {
				if !cfg.DebugMode {
					t.Error("expected DebugMode to be true")
				}
			},
		},
		{
			name: "-port sets deprecatedHTTPPort",
			args: []string{"-port=3000"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.deprecatedHTTPPort != "3000" {
					t.Errorf("expected deprecatedHTTPPort '3000', got %q", cfg.deprecatedHTTPPort)
				}
			},
		},
		{
			name: "TLS flags set TLS config",
			args: []string{"-tls-cert=/cert.pem", "-tls-key=/key.pem", "-tls-self-signed"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.TLS.Cert != "/cert.pem" {
					t.Errorf("expected TLS.Cert '/cert.pem', got %q", cfg.TLS.Cert)
				}
				if cfg.TLS.Key != "/key.pem" {
					t.Errorf("expected TLS.Key '/key.pem', got %q", cfg.TLS.Key)
				}
				if !cfg.TLS.SelfSigned {
					t.Error("expected TLS.SelfSigned to be true")
				}
			},
		},
		{
			name: "-tls-min-version=1.3 sets MinVersion",
			args: []string{"-tls-min-version=1.3"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.TLS.MinVersion != TLSVersion(tls.VersionTLS13) {
					t.Errorf("expected TLS.MinVersion TLS 1.3 (%d), got %d", tls.VersionTLS13, cfg.TLS.MinVersion)
				}
			},
		},
		{
			name: "multiple flags at once",
			args: []string{"-name=multi", "-namespace=multi-ns", "-secure", "-debug"},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Name != "multi" {
					t.Errorf("expected Name 'multi', got %q", cfg.Name)
				}
				if cfg.Namespace != "multi-ns" {
					t.Errorf("expected Namespace 'multi-ns', got %q", cfg.Namespace)
				}
				if !cfg.Secure {
					t.Error("expected Secure to be true")
				}
				if !cfg.DebugMode {
					t.Error("expected DebugMode to be true")
				}
			},
		},
		{
			name: "flags override env var defaults",
			args: []string{"-name=flag-value"},
			check: func(t *testing.T, cfg *Config) {
				// The config was initialized with Name="env-value" (see setup below),
				// but the flag should override it.
				if cfg.Name != "flag-value" {
					t.Errorf("expected flag to override env default, got Name %q", cfg.Name)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				Name:             "initial",
				Namespace:        "initial-ns",
				GatewayName:      "initial-gw",
				GatewayNamespace: "initial-gw-ns",
				TLS: TLSConfig{
					MinVersion: TLSVersion(tls.VersionTLS12),
				},
			}

			// For the "flags override env var defaults" test, set an initial
			// env-like value that the flag should override.
			if tt.name == "flags override env var defaults" {
				cfg.Name = "env-value"
			}

			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			cfg.bindFlags(fs)

			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("failed to parse flags: %v", err)
			}

			tt.check(t, cfg)
		})
	}
}

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
				DBConnectionURL:        "postgresql://localhost/test",
				Secure:                 false,
				APIKeyExpirationPolicy: "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "valid secure config with self-signed sets default address :8443",
			cfg: Config{
				DBConnectionURL:        "postgresql://localhost/test",
				TLS:                    TLSConfig{SelfSigned: true, MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyExpirationPolicy: "optional",
				APIKeyMaxExpirationDays: 30,
			},
		},
		{
			name: "valid secure config with certs",
			cfg: Config{
				DBConnectionURL:        "postgresql://localhost/test",
				TLS:                    TLSConfig{Cert: "/cert.pem", Key: "/key.pem", MinVersion: TLSVersion(tls.VersionTLS12)},
				APIKeyExpirationPolicy: "optional",
				APIKeyMaxExpirationDays: 30,
			},
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
				if !contains(err.Error(), tt.expectError) {
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

func TestConfig_Validate_APIKeyMaxExpirationDays(t *testing.T) {
	tests := []struct {
		name      string
		maxDays   int
		wantError bool
		errorMsg  string
	}{
		{
			name:      "valid minimum value",
			maxDays:   1,
			wantError: false,
		},
		{
			name:      "valid default value",
			maxDays:   30,
			wantError: false,
		},
		{
			name:      "valid large value",
			maxDays:   365,
			wantError: false,
		},
		{
			name:      "invalid zero value",
			maxDays:   0,
			wantError: true,
			errorMsg:  "must be at least 1",
		},
		{
			name:      "invalid negative value",
			maxDays:   -1,
			wantError: true,
			errorMsg:  "must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{
				DBConnectionURL:         "postgresql://test:test@localhost/test",
				APIKeyExpirationPolicy:  "optional",
				APIKeyMaxExpirationDays: tt.maxDays,
			}

			err := c.Validate()

			if tt.wantError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errorMsg)
					return
				}
				if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errorMsg, err.Error())
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
