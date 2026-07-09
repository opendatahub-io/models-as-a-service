package api_keys

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func TestNewPostgresStoreFromURL_InvalidScheme_NoCredentialLeak(t *testing.T) {
	log := logger.New(false)
	urlWithCreds := "mysql://admin:s3cret@db.example.com:5432/mydb"

	_, err := NewPostgresStoreFromURL(context.Background(), log, urlWithCreds, "test-tenant")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "s3cret", "error message must not contain the password")
	assert.NotContains(t, err.Error(), urlWithCreds, "error message must not contain the full URL")
	assert.Contains(t, err.Error(), "invalid database URL scheme")
}

func TestNewPostgresStoreFromURL_WhitespacePrefix_NoCredentialLeak(t *testing.T) {
	log := logger.New(false)
	urlWithCreds := "  mysql://admin:s3cret@db.example.com:5432/mydb"

	_, err := NewPostgresStoreFromURL(context.Background(), log, urlWithCreds, "test-tenant")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "s3cret", "error message must not contain the password")
}

func TestRedactDatabaseURL(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		databaseURL string
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "redacts user:password from message",
			message:     "connection failed: postgresql://admin:s3cret@host:5432/db",
			databaseURL: "postgresql://admin:s3cret@host:5432/db",
			wantContain: "[REDACTED]",
			wantAbsent:  "s3cret",
		},
		{
			name:        "preserves message when no userinfo",
			message:     "connection failed: postgresql://host:5432/db",
			databaseURL: "postgresql://host:5432/db",
			wantContain: "connection failed",
			wantAbsent:  "",
		},
		{
			name:        "handles unparseable URL gracefully",
			message:     "some error with bad url",
			databaseURL: "://invalid",
			wantContain: "some error with bad url",
			wantAbsent:  "",
		},
		{
			name:        "redacts user-only (no password)",
			message:     "error: postgresql://admin@host:5432/db",
			databaseURL: "postgresql://admin@host:5432/db",
			wantContain: "[REDACTED]",
			wantAbsent:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := redactDatabaseURL(tt.message, tt.databaseURL)
			if tt.wantContain != "" {
				assert.Contains(t, result, tt.wantContain)
			}
			if tt.wantAbsent != "" {
				assert.NotContains(t, result, tt.wantAbsent)
			}
		})
	}
}
