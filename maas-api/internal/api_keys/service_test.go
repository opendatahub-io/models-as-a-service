package api_keys_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func createTestService(t *testing.T) (*api_keys.Service, *api_keys.MockStore) {
	t.Helper()
	store := api_keys.NewMockStore()
	cfg := &config.Config{}
	svc := api_keys.NewServiceWithLogger(store, cfg, logger.Development())
	return svc, store
}

// ============================================================
// VALIDATE API KEY TESTS (CRITICAL - Security Function)
// ============================================================

func TestValidateAPIKey_ValidKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a valid API key
	keyID := "test-key-id"
	plainKey, hash := createTestAPIKey(t)
	username := "alice"
	groups := []string{"tier-premium", "system:authenticated"}

	err := store.AddKey(ctx, username, keyID, hash, "Test Key", "", groups, nil)
	require.NoError(t, err)

	// Validate the key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Valid)
	assert.Equal(t, username, result.UserID)
	assert.Equal(t, username, result.Username)
	assert.Equal(t, keyID, result.KeyID)
	assert.Equal(t, groups, result.Groups)
}

func TestValidateAPIKey_InvalidFormat(t *testing.T) {
	ctx := context.Background()
	svc, _ := createTestService(t)

	// Test various invalid formats
	invalidKeys := []string{
		"invalid-key",
		"maas_short",
		"wrong_prefix_1234567890abcdefghij",
		"",
		"just-random-text",
	}

	for _, invalidKey := range invalidKeys {
		result, err := svc.ValidateAPIKey(ctx, invalidKey)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.False(t, result.Valid, "Key should be invalid: %s", invalidKey)
		assert.Equal(t, "invalid key format", result.Reason)
	}
}

func TestValidateAPIKey_KeyNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := createTestService(t)

	// Generate a valid-format key that doesn't exist in the database
	plainKey, _ := createTestAPIKey(t)

	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.False(t, result.Valid)
	assert.Equal(t, "key not found", result.Reason)
}

func TestValidateAPIKey_RevokedKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create and immediately revoke a key
	keyID := "revoked-key-id"
	plainKey, hash := createTestAPIKey(t)
	username := "bob"
	groups := []string{"tier-free"}

	err := store.AddKey(ctx, username, keyID, hash, "Revoked Key", "", groups, nil)
	require.NoError(t, err)

	// Revoke the key
	err = store.Revoke(ctx, keyID)
	require.NoError(t, err)

	// Validate the revoked key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.False(t, result.Valid)
	assert.Equal(t, "key revoked or expired", result.Reason)
}

func TestValidateAPIKey_ExpiredKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a key that's already expired
	keyID := "expired-key-id"
	plainKey, hash := createTestAPIKey(t)
	username := "charlie"
	groups := []string{"tier-basic"}
	expiresAt := time.Now().Add(-24 * time.Hour) // Expired 1 day ago

	err := store.AddKey(ctx, username, keyID, hash, "Expired Key", "", groups, &expiresAt)
	require.NoError(t, err)

	// Validate the expired key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.False(t, result.Valid)
	assert.Equal(t, "key revoked or expired", result.Reason)
}

func TestValidateAPIKey_EmptyGroups(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a key with no groups (nil)
	keyID := "no-groups-key"
	plainKey, hash := createTestAPIKey(t)
	username := "dave"

	err := store.AddKey(ctx, username, keyID, hash, "No Groups Key", "", nil, nil)
	require.NoError(t, err)

	// Validate the key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Valid)
	assert.Equal(t, username, result.UserID)
	assert.NotNil(t, result.Groups, "Groups should be empty array, not nil")
	assert.Empty(t, result.Groups, "Groups should be empty")
}

func TestValidateAPIKey_UpdatesLastUsed(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a valid API key
	keyID := "last-used-key"
	plainKey, hash := createTestAPIKey(t)
	username := "eve"
	groups := []string{"tier-enterprise"}

	err := store.AddKey(ctx, username, keyID, hash, "Last Used Test", "", groups, nil)
	require.NoError(t, err)

	// Get initial metadata (last_used_at should be empty/nil)
	metaBefore, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Empty(t, metaBefore.LastUsedAt, "LastUsedAt should be empty initially")

	// Validate the key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	assert.True(t, result.Valid)

	// Give the async goroutine time to update last_used_at
	time.Sleep(50 * time.Millisecond)

	// Get metadata again - last_used_at should now be set
	metaAfter, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.NotEmpty(t, metaAfter.LastUsedAt, "LastUsedAt should be updated after validation")
}

// ============================================================
// SERVICE LAYER PASS-THROUGH TESTS
// ============================================================

func TestGetAPIKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a key
	keyID := "get-test-key"
	_, hash := createTestAPIKey(t)
	username := "alice"
	keyName := "Alice's Key"

	err := store.AddKey(ctx, username, keyID, hash, keyName, "Test description", nil, nil)
	require.NoError(t, err)

	// Get via service layer
	meta, err := svc.GetAPIKey(ctx, keyID)
	require.NoError(t, err)
	require.NotNil(t, meta)

	assert.Equal(t, keyID, meta.ID)
	assert.Equal(t, keyName, meta.Name)
	assert.Equal(t, username, meta.Username)
}

func TestGetAPIKey_NotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := createTestService(t)

	// Get non-existent key
	_, err := svc.GetAPIKey(ctx, "nonexistent-key")
	require.Error(t, err)
	assert.Equal(t, api_keys.ErrKeyNotFound, err)
}

func TestRevokeAPIKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a key
	keyID := "revoke-test-key"
	_, hash := createTestAPIKey(t)
	username := "bob"

	err := store.AddKey(ctx, username, keyID, hash, "Revoke Test", "", nil, nil)
	require.NoError(t, err)

	// Verify it's active
	meta, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Equal(t, api_keys.StatusActive, meta.Status)

	// Revoke via service layer
	err = svc.RevokeAPIKey(ctx, keyID)
	require.NoError(t, err)

	// Verify it's revoked
	meta, err = store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Equal(t, api_keys.StatusRevoked, meta.Status)
}

func TestServiceList(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create multiple keys for user
	username := "charlie"
	for i := 1; i <= 3; i++ {
		keyID := "list-test-key-" + string(rune('0'+i))
		_, hash := createTestAPIKey(t)
		err := store.AddKey(ctx, username, keyID, hash, "Key "+string(rune('0'+i)), "", nil, nil)
		require.NoError(t, err)
	}

	// List via service layer
	params := api_keys.PaginationParams{
		Limit:  10,
		Offset: 0,
	}
	result, err := svc.List(ctx, username, params, []string{api_keys.TokenStatusActive})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Len(t, result.Keys, 3, "Should return all 3 keys")
	assert.False(t, result.HasMore, "Should not have more results")
}

// ============================================================
// TEST HELPERS
// ============================================================

// createTestAPIKey generates a valid API key and its hash for testing.
func createTestAPIKey(t *testing.T) (string, string) {
	t.Helper()
	plainKey, hash, _, err := api_keys.GenerateAPIKey()
	require.NoError(t, err)
	return plainKey, hash
}
