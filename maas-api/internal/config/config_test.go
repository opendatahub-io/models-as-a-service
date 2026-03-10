package config_test

import (
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
)

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
			c := &config.Config{
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
