package api_keys_test

import (
	"context"
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
		err := store.AddKey(ctx, "user1", "key-id-1", "hash123", "my-key", "test key", []string{"system:authenticated", "premium-user"}, "sub-1", nil, false)
		require.NoError(t, err)

		// Verify key was added by fetching it
		key, err := store.Get(ctx, "key-id-1")
		require.NoError(t, err)
		assert.Equal(t, "my-key", key.Name)
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

	// Verify that revoking a key ID that doesn't exist in the store returns ErrKeyNotFound.
	t.Run("RevokeNonExistentKey", func(t *testing.T) {
		err := store.Revoke(ctx, "no-such-id")
		require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
	})

	// Verify that revoking an already-revoked key returns ErrKeyNotFound,
	// matching PostgreSQL behavior: only keys with status='active' can be revoked.
	t.Run("RevokeAlreadyRevokedKey", func(t *testing.T) {
		// Create a fresh key, revoke it, then try revoking again
		err := store.AddKey(ctx, "user3", "key-revoke-twice", "hash-revoke-twice", "revoke-twice", "", nil, "sub-1", nil, false)
		require.NoError(t, err)

		err = store.Revoke(ctx, "key-revoke-twice")
		require.NoError(t, err)

		// Second revoke should fail — key is no longer active
		err = store.Revoke(ctx, "key-revoke-twice")
		require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
	})

	t.Run("UpdateLastUsed", func(t *testing.T) {
		// Add another key for this test
		err := store.AddKey(ctx, "user2", "key-id-2", "hash456", "key2", "", []string{"system:authenticated", "free-user"}, "sub-2", nil, false)
		require.NoError(t, err)

		err = store.UpdateLastUsed(ctx, "key-id-2")
		require.NoError(t, err)

		key, err := store.GetByHash(ctx, "hash456")
		require.NoError(t, err)
		assert.NotEmpty(t, key.LastUsedAt)
	})
}

// TestInvalidateAll tests bulk revocation of all active keys for a given user.
// InvalidateAll revokes all keys with status='active' for a username and returns the count.
func TestInvalidateAll(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Verify that InvalidateAll revokes all active keys for the target user
	// while leaving other users' keys untouched.
	t.Run("BasicHappyPath", func(t *testing.T) {
		// Add 3 keys for alice, 2 for bob
		for i := range 3 {
			id := "alice-key-" + string(rune('a'+i))
			require.NoError(t, store.AddKey(ctx, "alice", id, "ahash"+id, "key-"+id, "", nil, "sub-1", nil, false))
		}
		for i := range 2 {
			id := "bob-key-" + string(rune('a'+i))
			require.NoError(t, store.AddKey(ctx, "bob", id, "bhash"+id, "key-"+id, "", nil, "sub-1", nil, false))
		}

		count, err := store.InvalidateAll(ctx, "alice")
		require.NoError(t, err)
		assert.Equal(t, 3, count)

		// Verify all of alice's keys are now revoked
		for i := range 3 {
			id := "alice-key-" + string(rune('a'+i))
			key, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, api_keys.StatusRevoked, key.Status, "alice's key %s should be revoked", id)
		}

		// Verify bob's keys are completely unaffected
		for i := range 2 {
			id := "bob-key-" + string(rune('a'+i))
			key, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, api_keys.StatusActive, key.Status, "bob's key %s should remain active", id)
		}
	})

	// Verify that InvalidateAll for a user with no keys returns count=0 and no error.
	t.Run("NoKeysForUser", func(t *testing.T) {
		count, err := store.InvalidateAll(ctx, "nobody")
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	// Verify that InvalidateAll only counts keys transitioning from active to revoked.
	// Pre-revoked keys should not be counted again.
	t.Run("MixedStatuses", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "carol", "c1", "ch1", "k1", "", nil, "sub-1", nil, false))
		require.NoError(t, s.AddKey(ctx, "carol", "c2", "ch2", "k2", "", nil, "sub-1", nil, false))
		require.NoError(t, s.AddKey(ctx, "carol", "c3", "ch3", "k3", "", nil, "sub-1", nil, false))

		// Revoke one key manually first
		require.NoError(t, s.Revoke(ctx, "c3"))

		// InvalidateAll should only revoke the 2 remaining active keys
		count, err := s.InvalidateAll(ctx, "carol")
		require.NoError(t, err)
		assert.Equal(t, 2, count, "should only revoke active keys, not already-revoked ones")
	})

	// Verify that calling InvalidateAll twice is idempotent:
	// the second call finds no active keys and returns count=0.
	t.Run("IdempotentSecondCall", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "dan", "d1", "dh1", "k1", "", nil, "sub-1", nil, false))

		count, err := s.InvalidateAll(ctx, "dan")
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		// Second call should be a no-op
		count, err = s.InvalidateAll(ctx, "dan")
		require.NoError(t, err)
		assert.Equal(t, 0, count, "second call should find no active keys")
	})
}

