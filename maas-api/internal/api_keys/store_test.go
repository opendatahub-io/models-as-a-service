package api_keys_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

//nolint:ireturn // Returns MetadataStore interface by design.
func createTestStore(t *testing.T) api_keys.MetadataStore {
	t.Helper()
	return api_keys.NewMockStore()
}

// TestStore tests legacy Add() method - NOTE: This method is DEPRECATED
// Legacy SA tokens are not stored in database in production - they use Kubernetes TokenReview
// These tests are kept for backward compatibility testing only.
func TestStore(t *testing.T) {
	t.Skip("Legacy Add() method is deprecated - SA tokens are not stored in database")

	// Tests removed - legacy SA token storage is not used in practice
	// Only hash-based keys (AddKey) are stored in database
}

func TestStoreValidation(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("TokenNotFound", func(t *testing.T) {
		_, err := store.Get(ctx, "nonexistent-jti")
		require.Error(t, err)
		assert.Equal(t, api_keys.ErrKeyNotFound, err)
	})

	// Legacy Add() validation tests removed - method is deprecated
	// SA tokens are not stored in database, validated via Kubernetes instead
}

func TestPostgresStoreFromURL(t *testing.T) {
	ctx := context.Background()
	testLogger := logger.Development()

	t.Run("InvalidURL", func(t *testing.T) {
		_, err := api_keys.NewPostgresStoreFromURL(ctx, testLogger, "mysql://localhost:3306/db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid database URL")
	})

	t.Run("EmptyURL", func(t *testing.T) {
		_, err := api_keys.NewPostgresStoreFromURL(ctx, testLogger, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid database URL")
	})
}

func TestAPIKeyOperations(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("AddKey", func(t *testing.T) {
		err := store.AddKey(ctx, "user1", "key-id-1", "hash123", "sk-oai-abc", "my-key", "test key", `["system:authenticated","premium-user"]`, nil)
		require.NoError(t, err)

		params := api_keys.PaginationParams{Limit: 10, Offset: 0}
		result, err := store.List(ctx, "user1", params)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 1)
		assert.Equal(t, "my-key", result.Keys[0].Name)
		assert.Equal(t, "sk-oai-abc", result.Keys[0].KeyPrefix)
		assert.False(t, result.HasMore)
	})

	t.Run("GetByHash", func(t *testing.T) {
		key, err := store.GetByHash(ctx, "hash123")
		require.NoError(t, err)
		assert.Equal(t, "my-key", key.Name)
		assert.Equal(t, "user1", key.Username)
		assert.Equal(t, []string{"system:authenticated", "premium-user"}, key.OriginalUserGroups)
	})

	t.Run("GetByHashNotFound", func(t *testing.T) {
		_, err := store.GetByHash(ctx, "nonexistent-hash")
		require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
	})

	t.Run("RevokeKey", func(t *testing.T) {
		err := store.Revoke(ctx, "key-id-1")
		require.NoError(t, err)

		// Getting by hash should now fail
		_, err = store.GetByHash(ctx, "hash123")
		require.ErrorIs(t, err, api_keys.ErrInvalidKey)
	})

	t.Run("UpdateLastUsed", func(t *testing.T) {
		// Add another key for this test
		err := store.AddKey(ctx, "user2", "key-id-2", "hash456", "sk-oai-def", "key2", "", `["system:authenticated","free-user"]`, nil)
		require.NoError(t, err)

		err = store.UpdateLastUsed(ctx, "key-id-2")
		require.NoError(t, err)

		key, err := store.GetByHash(ctx, "hash456")
		require.NoError(t, err)
		assert.NotEmpty(t, key.LastUsedAt)
	})
}

func TestList(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Create 125 test keys to test pagination
	const totalKeys = 125
	username := "paginated-user"

	for i := 1; i <= totalKeys; i++ {
		keyID := fmt.Sprintf("key-%d", i)
		keyHash := fmt.Sprintf("hash-%d", i)
		keyPrefix := fmt.Sprintf("sk-oai-%03d", i)
		name := fmt.Sprintf("Key %d", i)
		err := store.AddKey(ctx, username, keyID, keyHash, keyPrefix, name, "", `["system:authenticated"]`, nil)
		require.NoError(t, err)
	}

	t.Run("FirstPage", func(t *testing.T) {
		params := api_keys.PaginationParams{Limit: 50, Offset: 0}
		result, err := store.List(ctx, username, params)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 50, "should return exactly 50 keys")
		assert.True(t, result.HasMore, "should indicate more pages exist")
	})

	t.Run("LastPage", func(t *testing.T) {
		params := api_keys.PaginationParams{Limit: 50, Offset: 100}
		result, err := store.List(ctx, username, params)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 25, "should return remaining 25 keys")
		assert.False(t, result.HasMore, "should indicate no more pages")
	})

	t.Run("ValidationErrors", func(t *testing.T) {
		t.Run("NegativeLimit", func(t *testing.T) {
			params := api_keys.PaginationParams{Limit: 0, Offset: 0}
			_, err := store.List(ctx, username, params)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "limit must be between 1 and 100")
		})
	})
}
