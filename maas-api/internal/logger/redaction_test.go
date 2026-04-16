package logger_test

import (
	"net/http"
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/stretchr/testify/assert"
)

func TestRedactHeader(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		hashPrefix bool
		want       string
	}{
		{
			name:       "empty value without hash",
			value:      "",
			hashPrefix: false,
			want:       "absent",
		},
		{
			name:       "empty value with hash",
			value:      "",
			hashPrefix: true,
			want:       "absent",
		},
		{
			name:       "non-empty value without hash",
			value:      "Bearer secret-token",
			hashPrefix: false,
			want:       "present",
		},
		{
			name:       "non-empty value with hash prefix",
			value:      "Bearer secret-token",
			hashPrefix: true,
			want:       "present:sha256:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logger.RedactHeader(tt.value, tt.hashPrefix)
			if tt.hashPrefix && tt.value != "" {
				assert.Contains(t, got, tt.want, "Should contain prefix")
				assert.Len(t, got, len("present:sha256:12345678"), "Should have correct length")
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestRedactHeaders(t *testing.T) {
	headers := http.Header{
		"Authorization":   []string{"Bearer secret-token"},
		"X-API-Key":       []string{"api-key-123"},
		"Content-Type":    []string{"application/json"},
		"X-Request-ID":    []string{"abc-123"},
		"X-MaaS-Username": []string{"user@example.com"},
	}

	redacted := logger.RedactHeaders(headers, false)

	// Sensitive headers should be redacted
	assert.Equal(t, "present", redacted["Authorization"],
		"Authorization should be redacted")
	assert.Equal(t, "present", redacted["X-API-Key"],
		"X-API-Key should be redacted")

	// Non-sensitive headers should pass through
	assert.Equal(t, "application/json", redacted["Content-Type"],
		"Content-Type should pass through")
	assert.Equal(t, "abc-123", redacted["X-Request-ID"],
		"X-Request-ID should pass through")
	assert.Equal(t, "user@example.com", redacted["X-MaaS-Username"],
		"X-MaaS-Username should pass through")
}

func TestRedactHeaders_WithHashPrefix(t *testing.T) {
	headers := http.Header{
		"Authorization": []string{"Bearer token-1"},
	}

	redacted1 := logger.RedactHeaders(headers, true)

	// Change token value
	headers.Set("Authorization", "Bearer token-2")
	redacted2 := logger.RedactHeaders(headers, true)

	// Different tokens should produce different hash prefixes
	assert.NotEqual(t, redacted1["Authorization"], redacted2["Authorization"],
		"Different tokens should produce different hashes")

	// Both should start with expected prefix
	assert.Contains(t, redacted1["Authorization"], "present:sha256:")
	assert.Contains(t, redacted2["Authorization"], "present:sha256:")
}

func TestRedactHeaders_EmptyHeader(t *testing.T) {
	headers := http.Header{
		"Authorization": []string{""},
	}

	redacted := logger.RedactHeaders(headers, false)

	assert.Equal(t, "absent", redacted["Authorization"],
		"Empty Authorization should be 'absent'")
}

func TestIsSensitiveHeader(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		sensitive bool
	}{
		{"Authorization", "Authorization", true},
		{"X-API-Key", "X-API-Key", true},
		{"Cookie", "Cookie", true},
		{"Set-Cookie", "Set-Cookie", true},
		{"Content-Type", "Content-Type", false},
		{"X-Request-ID", "X-Request-ID", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logger.IsSensitiveHeader(tt.header)
			assert.Equal(t, tt.sensitive, got)
		})
	}
}
