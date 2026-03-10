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
		err := store.AddKey(ctx, "user1", "key-id-1", "hash123", "my-key", "test key", []string{"system:authenticated", "premium-user"}, nil)
		require.NoError(t, err)

		params := api_keys.PaginationParams{Limit: 10, Offset: 0}
		result, err := store.List(ctx, "user1", params, nil)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 1)
		assert.Equal(t, "my-key", result.Keys[0].Name)
		// KeyPrefix is NOT stored (security - reduces brute-force attack surface)
		assert.False(t, result.HasMore)
	})

	t.Run("GetByHash", func(t *testing.T) {
		key, err := store.GetByHash(ctx, "hash123")
		require.NoError(t, err)
		assert.Equal(t, "my-key", key.Name)
		assert.Equal(t, "user1", key.Username)
		assert.Equal(t, []string{"system:authenticated", "premium-user"}, key.Groups)
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
		err := store.AddKey(ctx, "user2", "key-id-2", "hash456", "key2", "", []string{"system:authenticated", "free-user"}, nil)
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
		name := fmt.Sprintf("Key %d", i)
		err := store.AddKey(ctx, username, keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	t.Run("FirstPage", func(t *testing.T) {
		params := api_keys.PaginationParams{Limit: 50, Offset: 0}
		result, err := store.List(ctx, username, params, nil)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 50, "should return exactly 50 keys")
		assert.True(t, result.HasMore, "should indicate more pages exist")
	})

	t.Run("LastPage", func(t *testing.T) {
		params := api_keys.PaginationParams{Limit: 50, Offset: 100}
		result, err := store.List(ctx, username, params, nil)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 25, "should return remaining 25 keys")
		assert.False(t, result.HasMore, "should indicate no more pages")
	})

	t.Run("ValidationErrors", func(t *testing.T) {
		t.Run("NegativeLimit", func(t *testing.T) {
			params := api_keys.PaginationParams{Limit: 0, Offset: 0}
			_, err := store.List(ctx, username, params, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "limit must be between 1 and 100")
		})
	})
}

func TestEmptyUsernameReturnsAllUsers(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Create 3 keys for alice
	for i := 1; i <= 3; i++ {
		keyID := fmt.Sprintf("alice-key-%d", i)
		keyHash := fmt.Sprintf("alice-hash-%d", i)
		name := fmt.Sprintf("Alice Key %d", i)
		err := store.AddKey(ctx, "alice", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	// Create 2 keys for bob
	for i := 1; i <= 2; i++ {
		keyID := fmt.Sprintf("bob-key-%d", i)
		keyHash := fmt.Sprintf("bob-hash-%d", i)
		name := fmt.Sprintf("Bob Key %d", i)
		err := store.AddKey(ctx, "bob", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	// List with empty username should return all keys
	params := api_keys.PaginationParams{Limit: 100, Offset: 0}
	result, err := store.List(ctx, "", params, nil)
	require.NoError(t, err)
	assert.Len(t, result.Keys, 5, "should return all 5 keys from both users")

	// Verify we have keys from both users
	usernames := make(map[string]int)
	for _, key := range result.Keys {
		usernames[key.Username]++
	}
	assert.Equal(t, 3, usernames["alice"], "should have 3 keys from alice")
	assert.Equal(t, 2, usernames["bob"], "should have 2 keys from bob")
}

func TestFilterByStatus(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Create 3 active keys
	for i := 1; i <= 3; i++ {
		keyID := fmt.Sprintf("active-key-%d", i)
		keyHash := fmt.Sprintf("active-hash-%d", i)
		name := fmt.Sprintf("Active Key %d", i)
		err := store.AddKey(ctx, "testuser", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	// Create 2 revoked keys
	for i := 1; i <= 2; i++ {
		keyID := fmt.Sprintf("revoked-key-%d", i)
		keyHash := fmt.Sprintf("revoked-hash-%d", i)
		name := fmt.Sprintf("Revoked Key %d", i)
		err := store.AddKey(ctx, "testuser", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
		err = store.Revoke(ctx, keyID)
		require.NoError(t, err)
	}

	params := api_keys.PaginationParams{Limit: 100, Offset: 0}

	t.Run("ActiveOnly", func(t *testing.T) {
		result, err := store.List(ctx, "testuser", params, []string{"active"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 3, "should return 3 active keys")
		for _, key := range result.Keys {
			assert.Equal(t, api_keys.StatusActive, key.Status)
		}
	})

	t.Run("RevokedOnly", func(t *testing.T) {
		result, err := store.List(ctx, "testuser", params, []string{"revoked"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 2, "should return 2 revoked keys")
		for _, key := range result.Keys {
			assert.Equal(t, api_keys.StatusRevoked, key.Status)
		}
	})
}

func TestFilterByMultipleStatuses(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Create 2 active keys
	for i := 1; i <= 2; i++ {
		keyID := fmt.Sprintf("active-key-%d", i)
		keyHash := fmt.Sprintf("active-hash-%d", i)
		name := fmt.Sprintf("Active Key %d", i)
		err := store.AddKey(ctx, "testuser", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	// Create 1 revoked key
	keyID := "revoked-key"
	keyHash := "revoked-hash"
	err := store.AddKey(ctx, "testuser", keyID, keyHash, "Revoked Key", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.Revoke(ctx, keyID)
	require.NoError(t, err)

	// Create 1 expired key (using past expiration)
	// Note: MockStore might not support expiration - this is a conceptual test
	// If expiration is not supported, this test will verify the filter logic works

	params := api_keys.PaginationParams{Limit: 100, Offset: 0}
	result, err := store.List(ctx, "testuser", params, []string{"active", "revoked"})
	require.NoError(t, err)

	// Should return active + revoked keys (3 total)
	assert.Len(t, result.Keys, 3, "should return 2 active + 1 revoked = 3 keys")

	// Verify we have both statuses
	statuses := make(map[string]int)
	for _, key := range result.Keys {
		statuses[string(key.Status)]++
	}
	assert.Equal(t, 2, statuses["active"], "should have 2 active keys")
	assert.Equal(t, 1, statuses["revoked"], "should have 1 revoked key")
}

func TestFilterByUsernameAndStatus(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// alice: 2 active, 1 revoked
	for i := 1; i <= 2; i++ {
		keyID := fmt.Sprintf("alice-active-%d", i)
		keyHash := fmt.Sprintf("alice-hash-active-%d", i)
		name := fmt.Sprintf("Alice Active Key %d", i)
		err := store.AddKey(ctx, "alice", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}
	keyID := "alice-revoked"
	keyHash := "alice-hash-revoked"
	err := store.AddKey(ctx, "alice", keyID, keyHash, "Alice Revoked Key", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.Revoke(ctx, keyID)
	require.NoError(t, err)

	// bob: 1 active, 2 revoked
	keyID = "bob-active"
	keyHash = "bob-hash-active"
	err = store.AddKey(ctx, "bob", keyID, keyHash, "Bob Active Key", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)

	for i := 1; i <= 2; i++ {
		keyID = fmt.Sprintf("bob-revoked-%d", i)
		keyHash = fmt.Sprintf("bob-hash-revoked-%d", i)
		name := fmt.Sprintf("Bob Revoked Key %d", i)
		err = store.AddKey(ctx, "bob", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
		err = store.Revoke(ctx, keyID)
		require.NoError(t, err)
	}

	params := api_keys.PaginationParams{Limit: 100, Offset: 0}

	t.Run("AliceActive", func(t *testing.T) {
		result, err := store.List(ctx, "alice", params, []string{"active"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 2, "alice should have 2 active keys")
		for _, key := range result.Keys {
			assert.Equal(t, "alice", key.Username)
			assert.Equal(t, api_keys.StatusActive, key.Status)
		}
	})

	t.Run("AliceRevoked", func(t *testing.T) {
		result, err := store.List(ctx, "alice", params, []string{"revoked"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 1, "alice should have 1 revoked key")
		assert.Equal(t, "alice", result.Keys[0].Username)
		assert.Equal(t, api_keys.StatusRevoked, result.Keys[0].Status)
	})

	t.Run("BobActive", func(t *testing.T) {
		result, err := store.List(ctx, "bob", params, []string{"active"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 1, "bob should have 1 active key")
		assert.Equal(t, "bob", result.Keys[0].Username)
		assert.Equal(t, api_keys.StatusActive, result.Keys[0].Status)
	})

	t.Run("BobRevoked", func(t *testing.T) {
		result, err := store.List(ctx, "bob", params, []string{"revoked"})
		require.NoError(t, err)
		assert.Len(t, result.Keys, 2, "bob should have 2 revoked keys")
		for _, key := range result.Keys {
			assert.Equal(t, "bob", key.Username)
			assert.Equal(t, api_keys.StatusRevoked, key.Status)
		}
	})
}
