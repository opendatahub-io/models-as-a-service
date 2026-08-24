package api_keys_test

import (
	"context"
	"testing"
	"time"

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
		_, err := api_keys.NewPostgresStoreFromURL(ctx, testLogger, "mysql://localhost:3306/db", "test-tenant")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid database URL")
	})

	t.Run("EmptyURL", func(t *testing.T) {
		_, err := api_keys.NewPostgresStoreFromURL(ctx, testLogger, "", "test-tenant")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid database URL")
	})
}

func TestAPIKeyOperations(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("AddKey", func(t *testing.T) {
		err := store.AddKey(ctx, "user1", "key-id-1", "hash123", "my-key", "test key", []string{"system:authenticated", "premium-user"}, "sub-1", "", nil, false, nil)
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
		err := store.AddKey(ctx, "user3", "key-revoke-twice", "hash-revoke-twice", "revoke-twice", "", nil, "sub-1", "", nil, false, nil)
		require.NoError(t, err)

		err = store.Revoke(ctx, "key-revoke-twice")
		require.NoError(t, err)

		// Second revoke should fail — key is no longer active
		err = store.Revoke(ctx, "key-revoke-twice")
		require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
	})

	t.Run("UpdateLastUsed", func(t *testing.T) {
		// Add another key for this test
		err := store.AddKey(ctx, "user2", "key-id-2", "hash456", "key2", "", []string{"system:authenticated", "free-user"}, "sub-2", "", nil, false, nil)
		require.NoError(t, err)

		err = store.UpdateLastUsed(ctx, "key-id-2")
		require.NoError(t, err)

		key, err := store.GetByHash(ctx, "hash456")
		require.NoError(t, err)
		assert.NotEmpty(t, key.LastUsedAt)
	})
}

// TestBulkRevoke tests bulk revocation of all active keys for a given scope.
// BulkRevoke revokes all keys with status='active' matching the username and/or
// subscription filter and returns the count.
func TestBulkRevoke(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("BasicHappyPath", func(t *testing.T) {
		for i := range 3 {
			id := "alice-key-" + string(rune('a'+i))
			require.NoError(t, store.AddKey(ctx, "alice", id, "ahash"+id, "key-"+id, "", nil, "sub-1", "", nil, false, nil))
		}
		for i := range 2 {
			id := "bob-key-" + string(rune('a'+i))
			require.NoError(t, store.AddKey(ctx, "bob", id, "bhash"+id, "key-"+id, "", nil, "sub-1", "", nil, false, nil))
		}

		count, err := store.BulkRevoke(ctx, "alice", "", "", false)
		require.NoError(t, err)
		assert.Equal(t, 3, count)

		for i := range 3 {
			id := "alice-key-" + string(rune('a'+i))
			key, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, api_keys.StatusRevoked, key.Status, "alice's key %s should be revoked", id)
		}

		for i := range 2 {
			id := "bob-key-" + string(rune('a'+i))
			key, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, api_keys.StatusActive, key.Status, "bob's key %s should remain active", id)
		}
	})

	t.Run("NoKeysForUser", func(t *testing.T) {
		count, err := store.BulkRevoke(ctx, "nobody", "", "", false)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("MixedStatuses", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "carol", "c1", "ch1", "k1", "", nil, "sub-1", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "carol", "c2", "ch2", "k2", "", nil, "sub-1", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "carol", "c3", "ch3", "k3", "", nil, "sub-1", "", nil, false, nil))

		require.NoError(t, s.Revoke(ctx, "c3"))

		count, err := s.BulkRevoke(ctx, "carol", "", "", false)
		require.NoError(t, err)
		assert.Equal(t, 2, count, "should only revoke active keys, not already-revoked ones")
	})

	t.Run("IdempotentSecondCall", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "dan", "d1", "dh1", "k1", "", nil, "sub-1", "", nil, false, nil))

		count, err := s.BulkRevoke(ctx, "dan", "", "", false)
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		count, err = s.BulkRevoke(ctx, "dan", "", "", false)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "second call should find no active keys")
	})

	t.Run("BySubscription", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "alice", "sa-1", "sah1", "k1", "", nil, "sub-alpha", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "bob", "sa-2", "sah2", "k2", "", nil, "sub-alpha", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "alice", "sb-1", "sbh1", "k3", "", nil, "sub-beta", "", nil, false, nil))

		count, err := s.BulkRevoke(ctx, "", "sub-alpha", "", false)
		require.NoError(t, err)
		assert.Equal(t, 2, count)

		key, _ := s.Get(ctx, "sb-1")
		assert.Equal(t, api_keys.StatusActive, key.Status, "sub-beta key should remain active")
	})

	t.Run("ByUsernameAndSubscription", func(t *testing.T) {
		s := createTestStore(t)
		defer s.Close()

		require.NoError(t, s.AddKey(ctx, "alice", "us-1", "ush1", "k1", "", nil, "sub-x", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "alice", "us-2", "ush2", "k2", "", nil, "sub-y", "", nil, false, nil))
		require.NoError(t, s.AddKey(ctx, "bob", "us-3", "ush3", "k3", "", nil, "sub-x", "", nil, false, nil))

		count, err := s.BulkRevoke(ctx, "alice", "sub-x", "", false)
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		key, _ := s.Get(ctx, "us-2")
		assert.Equal(t, api_keys.StatusActive, key.Status, "alice sub-y key should remain active")
		key, _ = s.Get(ctx, "us-3")
		assert.Equal(t, api_keys.StatusActive, key.Status, "bob sub-x key should remain active")
	})
}

func TestAddKeyWithTenant(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	t.Run("TenantRoundTripsViaGet", func(t *testing.T) {
		err := store.AddKey(ctx, "user1", "tenant-key-1", "thash1", "tenant-key", "", nil, "sub-1", "acme-corp", nil, false, nil)
		require.NoError(t, err)

		key, err := store.Get(ctx, "tenant-key-1")
		require.NoError(t, err)
		assert.Equal(t, "acme-corp", key.Tenant)
	})

	t.Run("EmptyTenantSentinel", func(t *testing.T) {
		err := store.AddKey(ctx, "user1", "tenant-key-2", "thash2", "no-tenant-key", "", nil, "sub-1", "", nil, false, nil)
		require.NoError(t, err)

		key, err := store.Get(ctx, "tenant-key-2")
		require.NoError(t, err)
		assert.Empty(t, key.Tenant)
	})

	t.Run("TenantRoundTripsViaGetByHash", func(t *testing.T) {
		err := store.AddKey(ctx, "user1", "tenant-key-3", "thash3", "hash-tenant-key", "", nil, "sub-1", "tenant-xyz", nil, false, nil)
		require.NoError(t, err)

		key, err := store.GetByHash(ctx, "thash3")
		require.NoError(t, err)
		assert.Equal(t, "tenant-xyz", key.Tenant)
	})
}

// TestSearchByTenant verifies that the store Search method correctly scopes
// results by tenant, returning only keys matching the specified tenant.
func TestSearchByTenant(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Add 2 keys for tenant-a
	require.NoError(t, store.AddKey(ctx, "user1", "sa-1", "shah1", "key-a1", "", nil, "sub-1", "tenant-a", nil, false, nil))
	require.NoError(t, store.AddKey(ctx, "user1", "sa-2", "shah2", "key-a2", "", nil, "sub-1", "tenant-a", nil, false, nil))
	// Add 1 key for tenant-b
	require.NoError(t, store.AddKey(ctx, "user1", "sb-1", "shbh1", "key-b1", "", nil, "sub-1", "tenant-b", nil, false, nil))
	// Add 1 key for tenant-c
	require.NoError(t, store.AddKey(ctx, "user1", "sc-1", "shch1", "key-c1", "", nil, "sub-1", "tenant-c", nil, false, nil))

	filters := api_keys.SearchFilters{}
	sortP := api_keys.SortParams{By: api_keys.DefaultSortBy, Order: api_keys.DefaultSortOrder}
	pagination := api_keys.PaginationParams{Limit: 50, Offset: 0}

	t.Run("TenantA_Returns2Keys", func(t *testing.T) {
		result, err := store.Search(ctx, "user1", "tenant-a", &filters, &sortP, &pagination)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 2)
	})

	t.Run("TenantB_Returns1Key", func(t *testing.T) {
		result, err := store.Search(ctx, "user1", "tenant-b", &filters, &sortP, &pagination)
		require.NoError(t, err)
		assert.Len(t, result.Keys, 1)
	})

	t.Run("NonexistentTenant_Returns0Keys", func(t *testing.T) {
		result, err := store.Search(ctx, "user1", "nonexistent", &filters, &sortP, &pagination)
		require.NoError(t, err)
		assert.Empty(t, result.Keys)
	})
}

// TestBulkRevoke_TenantScoped verifies that BulkRevoke only revokes keys
// within the specified tenant, leaving keys in other tenants active.
func TestBulkRevoke_TenantScoped(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	// Add 2 keys for alice in tenant-a
	require.NoError(t, store.AddKey(ctx, "alice", "ta-1", "tah1", "key-ta1", "", nil, "sub-1", "tenant-a", nil, false, nil))
	require.NoError(t, store.AddKey(ctx, "alice", "ta-2", "tah2", "key-ta2", "", nil, "sub-1", "tenant-a", nil, false, nil))
	// Add 2 keys for alice in tenant-b
	require.NoError(t, store.AddKey(ctx, "alice", "tb-1", "tbh1", "key-tb1", "", nil, "sub-1", "tenant-b", nil, false, nil))
	require.NoError(t, store.AddKey(ctx, "alice", "tb-2", "tbh2", "key-tb2", "", nil, "sub-1", "tenant-b", nil, false, nil))

	count, err := store.BulkRevoke(ctx, "alice", "", "tenant-a", false)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	for _, id := range []string{"ta-1", "ta-2"} {
		key, err := store.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, api_keys.StatusRevoked, key.Status, "tenant-a key %s should be revoked", id)
	}

	for _, id := range []string{"tb-1", "tb-2"} {
		key, err := store.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, api_keys.StatusActive, key.Status, "tenant-b key %s should remain active", id)
	}
}

func TestBulkRevoke_ExcludesExpiredKeys(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	pastExpiry := time.Now().Add(-time.Hour)
	futureExpiry := time.Now().Add(24 * time.Hour)

	require.NoError(t, store.AddKey(ctx, "alice", "active-1", "ah1", "key-a1", "", nil, "sub-1", "tenant-a", &futureExpiry, false, nil))
	require.NoError(t, store.AddKey(ctx, "alice", "expired-1", "ah2", "key-a2", "", nil, "sub-1", "tenant-a", &pastExpiry, false, nil))
	require.NoError(t, store.AddKey(ctx, "alice", "permanent-1", "ah3", "key-a3", "", nil, "sub-1", "tenant-a", nil, false, nil))

	count, err := store.BulkRevoke(ctx, "alice", "", "tenant-a", true)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "dry-run should count only active+permanent, not expired")

	count, err = store.BulkRevoke(ctx, "alice", "", "tenant-a", false)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "should revoke only active+permanent, not expired")
}

func TestBulkRevoke_EmptyScopeRejected(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	_, err := store.BulkRevoke(ctx, "", "", "tenant-a", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of username or subscription is required")

	_, err = store.BulkRevoke(ctx, "", "", "tenant-a", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of username or subscription is required")
}

func TestInvalidateTenant(t *testing.T) {
	ctx := t.Context()
	store := createTestStore(t)
	defer store.Close()

	require.NoError(t, store.AddKey(ctx, "alice", "tenant-a-1", "tenant-ah1", "key-ta1", "", nil, "sub-1", "tenant-a", nil, false, nil))
	require.NoError(t, store.AddKey(ctx, "bob", "tenant-a-2", "tenant-ah2", "key-ta2", "", nil, "sub-1", "tenant-a", nil, false, nil))
	require.NoError(t, store.AddKey(ctx, "alice", "tenant-b-1", "tenant-bh1", "key-tb1", "", nil, "sub-1", "tenant-b", nil, false, nil))

	count, err := store.InvalidateTenant(ctx, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	for _, id := range []string{"tenant-a-1", "tenant-a-2"} {
		key, err := store.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, api_keys.StatusRevoked, key.Status, "tenant-a key %s should be revoked", id)
	}

	key, err := store.Get(ctx, "tenant-b-1")
	require.NoError(t, err)
	assert.Equal(t, api_keys.StatusActive, key.Status, "tenant-b key should remain active")

	count, err = store.InvalidateTenant(ctx, "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}
