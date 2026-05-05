package config //nolint:testpackage // Testing unexported loadTLSConfig() function

import (
	"crypto/tls"
	"flag"
	"os"
	"testing"
)

// TestTLSMinVersion_Precedence verifies that CLI flags take precedence over environment variables.
// This test addresses the bug where TLS_MIN_VERSION incorrectly overrode --tls-min-version CLI flag.
func TestTLSMinVersion_Precedence(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		cliValue    string
		expected    uint16
		description string
	}{
		{
			name:        "CLI flag overrides environment variable",
			envValue:    "1.2",
			cliValue:    "1.3",
			expected:    tls.VersionTLS13,
			description: "When both env var (1.2) and CLI flag (1.3) are set, CLI flag should win",
		},
		{
			name:        "Environment variable used when no CLI flag",
			envValue:    "1.3",
			cliValue:    "",
			expected:    tls.VersionTLS13,
			description: "When only env var is set, it should be used",
		},
		{
			name:        "Default used when neither env var nor CLI flag set",
			envValue:    "",
			cliValue:    "",
			expected:    tls.VersionTLS12,
			description: "When neither env var nor CLI flag is set, default (1.2) should be used",
		},
		{
			name:        "Invalid env var falls back to default, CLI flag overrides",
			envValue:    "invalid",
			cliValue:    "1.3",
			expected:    tls.VersionTLS13,
			description: "When env var is invalid and CLI flag is set, CLI flag should be used",
		},
		{
			name:        "Invalid env var falls back to default when no CLI flag",
			envValue:    "invalid",
			cliValue:    "",
			expected:    tls.VersionTLS12,
			description: "When env var is invalid and no CLI flag, default (1.2) should be used",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup: Clear and set environment variable if needed
			if tt.envValue != "" {
				t.Setenv("TLS_MIN_VERSION", tt.envValue)
			} else {
				os.Unsetenv("TLS_MIN_VERSION")
			}
			// t.Setenv() automatically cleans up, no defer needed

			// Load config (reads env var as fallback)
			cfg := loadTLSConfig()

			// Simulate production flag binding/parsing.
			fs := flag.NewFlagSet(tt.name, flag.ContinueOnError)
			cfg.bindFlags(fs)
			if tt.cliValue != "" {
				if err := fs.Parse([]string{"--tls-min-version", tt.cliValue}); err != nil {
					t.Fatalf("Failed to parse CLI value %q: %v", tt.cliValue, err)
				}
			} else if err := fs.Parse(nil); err != nil {
				t.Fatalf("Failed to parse empty CLI args: %v", err)
			}

			// Validate (should NOT override MinVersion anymore)
			if err := cfg.validate(); err != nil {
				t.Fatalf("Validation failed: %v", err)
			}

			// Assert: Check that the correct value is used
			if cfg.MinVersion.Value() != tt.expected {
				t.Errorf("%s\nExpected TLS version %d, got %d", tt.description, tt.expected, cfg.MinVersion.Value())
			}
		})
	}
}

// TestTLSVersion_SetAndString tests the TLSVersion type's Set and String methods.
func TestTLSVersion_SetAndString(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedVal uint16
		expectedStr string
		expectError bool
	}{
		{
			name:        "Set TLS 1.2",
			input:       "1.2",
			expectedVal: tls.VersionTLS12,
			expectedStr: "1.2",
			expectError: false,
		},
		{
			name:        "Set TLS 1.3",
			input:       "1.3",
			expectedVal: tls.VersionTLS13,
			expectedStr: "1.3",
			expectError: false,
		},
		{
			name:        "Invalid version returns error",
			input:       "1.1",
			expectedVal: 0,
			expectedStr: "",
			expectError: true,
		},
		{
			name:        "Invalid string returns error",
			input:       "invalid",
			expectedVal: 0,
			expectedStr: "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v TLSVersion
			err := v.Set(tt.input)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for input %q, but got none", tt.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if v.Value() != tt.expectedVal {
				t.Errorf("Expected value %d, got %d", tt.expectedVal, v.Value())
			}

			if v.String() != tt.expectedStr {
				t.Errorf("Expected string %q, got %q", tt.expectedStr, v.String())
			}
		})
	}
}

// TestLoadTLSConfig_EnvironmentVariables tests that loadTLSConfig correctly reads all TLS env vars.
func TestLoadTLSConfig_EnvironmentVariables(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		expected TLSConfig
	}{
		{
			name: "All TLS env vars set",
			env: map[string]string{
				"TLS_CERT":        "/path/to/cert.pem",
				"TLS_KEY":         "/path/to/key.pem",
				"TLS_SELF_SIGNED": "true",
				"TLS_MIN_VERSION": "1.3",
			},
			expected: TLSConfig{
				Cert:       "/path/to/cert.pem",
				Key:        "/path/to/key.pem",
				SelfSigned: true,
				MinVersion: TLSVersion(tls.VersionTLS13),
			},
		},
		{
			name: "Only cert and key set",
			env: map[string]string{
				"TLS_CERT": "/path/to/cert.pem",
				"TLS_KEY":  "/path/to/key.pem",
			},
			expected: TLSConfig{
				Cert:       "/path/to/cert.pem",
				Key:        "/path/to/key.pem",
				SelfSigned: false,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
		},
		{
			name: "Only self-signed set",
			env: map[string]string{
				"TLS_SELF_SIGNED": "true",
			},
			expected: TLSConfig{
				Cert:       "",
				Key:        "",
				SelfSigned: true,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
		},
		{
			name: "No TLS env vars set - defaults",
			env:  map[string]string{},
			expected: TLSConfig{
				Cert:       "",
				Key:        "",
				SelfSigned: false,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup: Clear all TLS env vars
			envVars := []string{"TLS_CERT", "TLS_KEY", "TLS_SELF_SIGNED", "TLS_MIN_VERSION"}
			for _, key := range envVars {
				os.Unsetenv(key)
			}

			// Set test env vars
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			// t.Setenv() automatically cleans up, no defer needed

			// Load config
			cfg := loadTLSConfig()

			// Assert
			if cfg.Cert != tt.expected.Cert {
				t.Errorf("Expected Cert %q, got %q", tt.expected.Cert, cfg.Cert)
			}
			if cfg.Key != tt.expected.Key {
				t.Errorf("Expected Key %q, got %q", tt.expected.Key, cfg.Key)
			}
			if cfg.SelfSigned != tt.expected.SelfSigned {
				t.Errorf("Expected SelfSigned %v, got %v", tt.expected.SelfSigned, cfg.SelfSigned)
			}
			if cfg.MinVersion.Value() != tt.expected.MinVersion.Value() {
				t.Errorf("Expected MinVersion %d, got %d", tt.expected.MinVersion.Value(), cfg.MinVersion.Value())
			}
		})
	}
}

// TestTLSConfig_Validate tests the validate method.
func TestTLSConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		config      TLSConfig
		expectError bool
		errorMsg    string
	}{
		{
			name: "Valid config with cert and key",
			config: TLSConfig{
				Cert:       "/path/to/cert.pem",
				Key:        "/path/to/key.pem",
				SelfSigned: false,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
			expectError: false,
		},
		{
			name: "Valid config with self-signed",
			config: TLSConfig{
				Cert:       "",
				Key:        "",
				SelfSigned: true,
				MinVersion: TLSVersion(tls.VersionTLS13),
			},
			expectError: false,
		},
		{
			name: "Invalid - cert without key",
			config: TLSConfig{
				Cert:       "/path/to/cert.pem",
				Key:        "",
				SelfSigned: false,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
			expectError: true,
			errorMsg:    "--tls-cert and --tls-key must both be provided together",
		},
		{
			name: "Invalid - key without cert",
			config: TLSConfig{
				Cert:       "",
				Key:        "/path/to/key.pem",
				SelfSigned: false,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
			expectError: true,
			errorMsg:    "--tls-cert and --tls-key must both be provided together",
		},
		{
			name: "Cert and key override self-signed",
			config: TLSConfig{
				Cert:       "/path/to/cert.pem",
				Key:        "/path/to/key.pem",
				SelfSigned: true,
				MinVersion: TLSVersion(tls.VersionTLS12),
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure TLS_MIN_VERSION env var is not set for these tests
			os.Unsetenv("TLS_MIN_VERSION")

			err := tt.config.validate()

			if tt.expectError {
				if err == nil {
					t.Error("Expected error, but got none")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("Expected error message %q, got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// Check that cert+key override self-signed
				if tt.config.HasCerts() && tt.config.SelfSigned {
					t.Error("validate() should set SelfSigned=false when certs are provided, but it's still true")
				}
			}
		})
	}
}
