package api_keys_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func TestNewPostgresStoreFromURL_InvalidScheme_NoCredentialLeak(t *testing.T) {
	log := logger.New(false)
	urlWithCreds := "mysql://admin:s3cret@db.example.com:5432/mydb" //nolint:gosec // test fixture with fake credentials

	_, err := api_keys.NewPostgresStoreFromURL(context.Background(), log, urlWithCreds, "test-tenant")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "s3cret", "error message must not contain the password")
	assert.NotContains(t, err.Error(), urlWithCreds, "error message must not contain the full URL")
	assert.Contains(t, err.Error(), "invalid database URL scheme")
}

func TestNewPostgresStoreFromURL_WhitespacePrefix_NoCredentialLeak(t *testing.T) {
	log := logger.New(false)
	urlWithCreds := "  mysql://admin:s3cret@db.example.com:5432/mydb" //nolint:gosec // test fixture with fake credentials

	_, err := api_keys.NewPostgresStoreFromURL(context.Background(), log, urlWithCreds, "test-tenant")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "s3cret", "error message must not contain the password")
}
