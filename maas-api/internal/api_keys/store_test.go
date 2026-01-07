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

func createTestStore(t *testing.T) *api_keys.SQLStore {
	t.Helper()
	ctx := context.Background()
	testLogger := logger.Development()
	store, err := api_keys.NewSQLiteStore(ctx, testLogger, ":memory:")
	require.NoError(t, err, "failed to create test store")
	return store
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

	t.Run("MarkTokensAsExpiredForUser", func(t *testing.T) {
		err := store.InvalidateAll(ctx, "user1")
		require.NoError(t, err)

		tokens, err := store.List(ctx, "user1")
		require.NoError(t, err)
		assert.Len(t, tokens, 2)
		for _, tok := range tokens {
			assert.Equal(t, api_keys.TokenStatusExpired, tok.Status)
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
		assert.Equal(t, api_keys.ErrTokenNotFound, err)
	})
}

func TestSQLiteStore(t *testing.T) {
	ctx := context.Background()
	testLogger := logger.Development()

	t.Run("InMemory", func(t *testing.T) {
		store, err := api_keys.NewSQLiteStore(ctx, testLogger, ":memory:")
		require.NoError(t, err)
		defer store.Close()

		tokens, err := store.List(ctx, "user")
		require.NoError(t, err)
		assert.Empty(t, tokens)
	})

	t.Run("EmptyPath", func(t *testing.T) {
		// Empty path should default to in-memory
		store, err := api_keys.NewSQLiteStore(ctx, testLogger, "")
		require.NoError(t, err)
		defer store.Close()

		tokens, err := store.List(ctx, "user")
		require.NoError(t, err)
		assert.Empty(t, tokens)
	})
}

func TestExternalStore(t *testing.T) {
	ctx := context.Background()
	testLogger := logger.Development()

	t.Run("InvalidURL", func(t *testing.T) {
		_, err := api_keys.NewExternalStore(ctx, testLogger, "mysql://localhost:3306/db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported external database URL")
	})

	t.Run("EmptyURL", func(t *testing.T) {
		_, err := api_keys.NewExternalStore(ctx, testLogger, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported external database URL")
	})
}
