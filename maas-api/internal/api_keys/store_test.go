package api_keys_test

import (
	"context"
	"testing"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestStore(t *testing.T) api_keys.MetadataStore {
	t.Helper()
	return api_keys.NewMockStore()
}

func TestStore(t *testing.T) {
	ctx := t.Context()

	store := createTestStore(t)
	defer store.Close()

	t.Run("AddTokenMetadata", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "jti1",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			},
			Name: "token1",
		}
		err := store.Add(ctx, "user1", apiKey)
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user1")
		require.NoError(t, err)
		assert.Len(t, tokens, 1)
		assert.Equal(t, "token1", tokens[0].Name)
		assert.Equal(t, api_keys.TokenStatusActive, tokens[0].Status)
	})

	t.Run("AddSecondToken", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "jti2",
				ExpiresAt: time.Now().Add(2 * time.Hour).Unix(),
			},
			Name: "token2",
		}
		err := store.Add(ctx, "user1", apiKey)
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user1")
		require.NoError(t, err)
		assert.Len(t, tokens, 2)
	})

	t.Run("GetTokensForDifferentUser", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "jti3",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			},
			Name: "token3",
		}
		err := store.Add(ctx, "user2", apiKey)
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user2")
		require.NoError(t, err)
		assert.Len(t, tokens, 1)
		assert.Equal(t, "token3", tokens[0].Name)
	})

	t.Run("MarkTokensAsRevokedForUser", func(t *testing.T) {
		err := store.InvalidateAll(ctx, "user1")
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user1")
		require.NoError(t, err)
		assert.Len(t, tokens, 2)
		for _, tok := range tokens {
			assert.Equal(t, api_keys.TokenStatusRevoked, tok.Status)
		}

		// User2 should still exist
		tokens2, err := store.List(ctx, "user2")
		require.NoError(t, err)
		assert.Len(t, tokens2, 1)
	})

	t.Run("GetToken", func(t *testing.T) {
		gotToken, err := store.Get(ctx, "jti3")
		require.NoError(t, err)
		assert.NotNil(t, gotToken)
		assert.Equal(t, "token3", gotToken.Name)
	})

	t.Run("ExpiredTokenStatus", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "jti-expired",
				ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
			},
			Name: "expired-token",
		}
		err := store.Add(ctx, "user4", apiKey)
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user4")
		require.NoError(t, err)
		assert.Len(t, tokens, 1)
		assert.Equal(t, api_keys.TokenStatusExpired, tokens[0].Status)

		// Get single token check
		gotToken, err := store.Get(ctx, "jti-expired")
		require.NoError(t, err)
		assert.Equal(t, api_keys.TokenStatusExpired, gotToken.Status)
	})
}

func TestStoreValidation(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("EmptyJTI", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			},
			Name: "token-no-jti",
		}
		err := store.Add(ctx, "user1", apiKey)
		require.Error(t, err)
		assert.ErrorIs(t, err, api_keys.ErrEmptyJTI)
	})

	t.Run("EmptyName", func(t *testing.T) {
		apiKey := &api_keys.APIKey{
			Token: token.Token{
				JTI:       "some-jti",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			},
			Name: "",
		}
		err := store.Add(ctx, "user1", apiKey)
		require.Error(t, err)
		assert.ErrorIs(t, err, api_keys.ErrEmptyName)
	})

	t.Run("TokenNotFound", func(t *testing.T) {
		_, err := store.Get(ctx, "nonexistent-jti")
		require.Error(t, err)
		assert.Equal(t, api_keys.ErrKeyNotFound, err)
	})
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

func TestPermanentKeyOperations(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("AddPermanentKey", func(t *testing.T) {
		err := store.AddPermanentKey(ctx, "user1", "key-id-1", "hash123", "sk-oai-abc", "my-key", "test key", `["system:authenticated","premium-user"]`, nil)
		require.NoError(t, err)

		keys, err := store.List(ctx, "user1")
		require.NoError(t, err)
		assert.Len(t, keys, 1)
		assert.Equal(t, "my-key", keys[0].Name)
		assert.Equal(t, "sk-oai-abc", keys[0].KeyPrefix)
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
		err := store.AddPermanentKey(ctx, "user2", "key-id-2", "hash456", "sk-oai-def", "key2", "", `["system:authenticated","free-user"]`, nil)
		require.NoError(t, err)

		err = store.UpdateLastUsed(ctx, "key-id-2")
		require.NoError(t, err)

		key, err := store.GetByHash(ctx, "hash456")
		require.NoError(t, err)
		assert.NotEmpty(t, key.LastUsedAt)
	})
}
