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
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

type serviceTestSubSelector struct{}

func (serviceTestSubSelector) Select(_ []string, _ string, requested string, _ string) (*subscription.SelectResponse, error) {
	if requested != "" {
		return &subscription.SelectResponse{Name: requested, Phase: "Active"}, nil
	}
	return &subscription.SelectResponse{Name: "default-sub", Phase: "Active"}, nil
}

func (serviceTestSubSelector) SelectHighestPriority(_ []string, _ string) (*subscription.SelectResponse, error) {
	return &subscription.SelectResponse{Name: "default-sub", Phase: "Active"}, nil
}

func createTestService(t *testing.T) (*api_keys.Service, *api_keys.MockStore) {
	t.Helper()
	store := api_keys.NewMockStore()
	cfg := &config.Config{}
	svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())
	return svc, store
}

// ============================================================
// VALIDATE API KEY TESTS (CRITICAL - Security Function)
// ============================================================

func TestValidateAPIKey_ValidKey(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a valid API key with UUID (matches production database behavior)
	keyID := "550e8400-e29b-41d4-a716-446655440000"
	plainKey, hash := createTestAPIKey(t)
	username := "alice"
	groups := []string{"tier-premium", "system:authenticated"}

	err := store.AddKey(ctx, username, keyID, hash, "Test Key", "", groups, "default-sub", nil, false)
	require.NoError(t, err)

	// Validate the key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Valid)
	assert.Equal(t, keyID, result.UserID) // UserID is the database-assigned key ID (UUID)
	assert.Equal(t, username, result.Username)
	assert.Equal(t, keyID, result.KeyID)
	assert.Equal(t, groups, result.Groups)
	assert.Equal(t, "default-sub", result.Subscription)
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
	keyID := "550e8400-e29b-41d4-a716-446655440001"
	plainKey, hash := createTestAPIKey(t)
	username := "bob"
	groups := []string{"tier-free"}

	err := store.AddKey(ctx, username, keyID, hash, "Revoked Key", "", groups, "default-sub", nil, false)
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
	keyID := "550e8400-e29b-41d4-a716-446655440002"
	plainKey, hash := createTestAPIKey(t)
	username := "charlie"
	groups := []string{"tier-basic"}
	expiresAt := time.Now().Add(-24 * time.Hour) // Expired 1 day ago

	err := store.AddKey(ctx, username, keyID, hash, "Expired Key", "", groups, "default-sub", &expiresAt, false)
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
	keyID := "550e8400-e29b-41d4-a716-446655440003"
	plainKey, hash := createTestAPIKey(t)
	username := "dave"

	err := store.AddKey(ctx, username, keyID, hash, "No Groups Key", "", nil, "default-sub", nil, false)
	require.NoError(t, err)

	// Validate the key
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.True(t, result.Valid)
	assert.Equal(t, keyID, result.UserID) // UserID is the database-assigned key ID (UUID)
	assert.NotNil(t, result.Groups, "Groups should be empty array, not nil")
	assert.Empty(t, result.Groups, "Groups should be empty")
}

func TestValidateAPIKey_UpdatesLastUsed(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create a valid API key
	keyID := "550e8400-e29b-41d4-a716-446655440004"
	plainKey, hash := createTestAPIKey(t)
	username := "eve"
	groups := []string{"tier-enterprise"}

	err := store.AddKey(ctx, username, keyID, hash, "Last Used Test", "", groups, "default-sub", nil, false)
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
	keyID := "550e8400-e29b-41d4-a716-446655440005"
	_, hash := createTestAPIKey(t)
	username := "alice"
	keyName := "Alice's Key"

	err := store.AddKey(ctx, username, keyID, hash, keyName, "Test description", nil, "default-sub", nil, false)
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
	keyID := "550e8400-e29b-41d4-a716-446655440006"
	_, hash := createTestAPIKey(t)
	username := "bob"

	err := store.AddKey(ctx, username, keyID, hash, "Revoke Test", "", nil, "default-sub", nil, false)
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

// ============================================================
// REVOKE API KEY - ERROR PATHS
// ============================================================

// TestRevokeAPIKey_NotFound verifies that revoking a non-existent key
// propagates ErrKeyNotFound from the store through the service layer.
func TestRevokeAPIKey_NotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := createTestService(t)

	err := svc.RevokeAPIKey(ctx, "nonexistent-key-id")
	require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
}

// TestRevokeAPIKey_AlreadyRevoked verifies that revoking a key twice
// returns ErrKeyNotFound on the second attempt (matches PostgreSQL behavior:
// UPDATE ... WHERE status='active' affects 0 rows).
func TestRevokeAPIKey_AlreadyRevoked(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	keyID := "double-revoke-key"
	_, hash := createTestAPIKey(t)
	require.NoError(t, store.AddKey(ctx, "alice", keyID, hash, "Double Revoke", "", nil, "default-sub", nil, false))

	// First revoke succeeds
	require.NoError(t, svc.RevokeAPIKey(ctx, keyID))

	// Second revoke returns not found (key is no longer active)
	err := svc.RevokeAPIKey(ctx, keyID)
	require.ErrorIs(t, err, api_keys.ErrKeyNotFound)
}

// TestRevokeAPIKey_ThenValidate verifies the full revoke-then-validate flow:
// after revoking via the service layer, ValidateAPIKey should return invalid
// with reason "key revoked or expired".
func TestRevokeAPIKey_ThenValidate(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	keyID := "revoke-validate-key"
	plainKey, hash := createTestAPIKey(t)
	require.NoError(t, store.AddKey(ctx, "eve", keyID, hash, "Revoke Then Validate", "", []string{"users"}, "default-sub", nil, false))

	// Revoke via service
	require.NoError(t, svc.RevokeAPIKey(ctx, keyID))

	// Validate should report the key as invalid
	result, err := svc.ValidateAPIKey(ctx, plainKey)
	require.NoError(t, err)
	assert.False(t, result.Valid)
	assert.Equal(t, "key revoked or expired", result.Reason)
}

// ============================================================
// BULK REVOKE API KEYS (SERVICE LAYER)
// ============================================================

// TestBulkRevokeAPIKeys tests the service-level BulkRevokeAPIKeys function
// which revokes all active keys for a given username.
func TestBulkRevokeAPIKeys(t *testing.T) {
	// Verify that bulk revoke revokes all active keys for the target user
	// and returns the correct count.
	t.Run("HappyPath", func(t *testing.T) {
		ctx := context.Background()
		svc, store := createTestService(t)

		// Create 3 keys for alice
		for i := range 3 {
			_, hash := createTestAPIKey(t)
			id := "bulk-key-" + string(rune('a'+i))
			require.NoError(t, store.AddKey(ctx, "alice", id, hash, "Key "+id, "", nil, "default-sub", nil, false))
		}

		count, err := svc.BulkRevokeAPIKeys(ctx, "alice")
		require.NoError(t, err)
		assert.Equal(t, 3, count)

		// Verify all keys are revoked
		for i := range 3 {
			id := "bulk-key-" + string(rune('a'+i))
			meta, err := store.Get(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, api_keys.StatusRevoked, meta.Status, "key %s should be revoked", id)
		}
	})

	// Verify that bulk revoke for a user with no keys returns count=0 and no error.
	t.Run("NoKeysToRevoke", func(t *testing.T) {
		ctx := context.Background()
		svc, _ := createTestService(t)

		count, err := svc.BulkRevokeAPIKeys(ctx, "nobody")
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	// Verify that calling bulk revoke twice is idempotent:
	// the second call returns count=0 because all keys are already revoked.
	t.Run("Idempotent", func(t *testing.T) {
		ctx := context.Background()
		svc, store := createTestService(t)

		_, hash := createTestAPIKey(t)
		require.NoError(t, store.AddKey(ctx, "bob", "idem-key", hash, "Idempotent Key", "", nil, "default-sub", nil, false))

		count, err := svc.BulkRevokeAPIKeys(ctx, "bob")
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		// Second call: no active keys left
		count, err = svc.BulkRevokeAPIKeys(ctx, "bob")
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	// Verify that empty username is rejected with an error.
	t.Run("EmptyUsername", func(t *testing.T) {
		ctx := context.Background()
		svc, _ := createTestService(t)

		_, err := svc.BulkRevokeAPIKeys(ctx, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "username is required")
	})
}

// TestBulkRevokeAPIKeys_ThenValidateAll verifies that after bulk revoking,
// all previously valid keys are rejected by ValidateAPIKey.
func TestBulkRevokeAPIKeys_ThenValidateAll(t *testing.T) {
	ctx := context.Background()
	svc, store := createTestService(t)

	// Create 3 keys, keep their plaintext for validation
	plainKeys := make([]string, 3)
	for i := range 3 {
		plain, hash := createTestAPIKey(t)
		plainKeys[i] = plain
		id := "bulk-validate-" + string(rune('a'+i))
		require.NoError(t, store.AddKey(ctx, "carol", id, hash, "Key "+id, "", []string{"users"}, "default-sub", nil, false))
	}

	// Bulk revoke all of carol's keys
	count, err := svc.BulkRevokeAPIKeys(ctx, "carol")
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	// Validate each key — all should be rejected
	for i, plain := range plainKeys {
		result, err := svc.ValidateAPIKey(ctx, plain)
		require.NoError(t, err)
		assert.False(t, result.Valid, "key %d should be invalid after bulk revoke", i)
		assert.Equal(t, "key revoked or expired", result.Reason)
	}
}

// ============================================================
// MAX EXPIRATION VALIDATION TESTS
// ============================================================

func TestCreateAPIKey_MaxExpirationLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("WithinLimit", func(t *testing.T) {
		store := api_keys.NewMockStore()
		cfg := &config.Config{
			APIKeyMaxExpirationDays: 30, // 30 days max
		}
		svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())

		// Request 7 days - should succeed
		expiresIn := 7 * 24 * time.Hour
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", &expiresIn, false, "")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.NotEmpty(t, result.Key)
	})

	t.Run("ExceedsLimit", func(t *testing.T) {
		store := api_keys.NewMockStore()
		cfg := &config.Config{
			APIKeyMaxExpirationDays: 30, // 30 days max
		}
		svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())

		// Request 60 days - should fail
		expiresIn := 60 * 24 * time.Hour
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", &expiresIn, false, "")

		require.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "exceeds maximum allowed")
		assert.Contains(t, err.Error(), "30 days")
	})

	t.Run("ExactlyAtLimit", func(t *testing.T) {
		store := api_keys.NewMockStore()
		cfg := &config.Config{
			APIKeyMaxExpirationDays: 30, // 30 days max
		}
		svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())

		// Request exactly 30 days - should succeed
		expiresIn := 30 * 24 * time.Hour
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", &expiresIn, false, "")

		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("NoExpirationRequested", func(t *testing.T) {
		store := api_keys.NewMockStore()
		cfg := &config.Config{
			APIKeyMaxExpirationDays: 30, // 30 days max
		}
		svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())

		// No expiration requested - should default to APIKeyMaxExpirationDays (30 days)
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", nil, false, "")

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.ExpiresAt, "should default to max expiration when not provided")
	})

	// Regression test for CWE-613: ensure default max is enforced when config is nil/zero
	t.Run("DefaultConfigEnforcesMaxExpiration", func(t *testing.T) {
		store := api_keys.NewMockStore()
		// nil config or zero APIKeyMaxExpirationDays should fall back to DefaultAPIKeyMaxExpirationDays (90 days)
		svc := api_keys.NewServiceWithLogger(store, nil, serviceTestSubSelector{}, logger.Development())

		// Request 365 days - should fail because default max is 90 days
		expiresIn := 365 * 24 * time.Hour
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", &expiresIn, false, "")

		require.Error(t, err, "should reject expiration exceeding default max (90 days)")
		assert.Nil(t, result)
		require.ErrorIs(t, err, api_keys.ErrExpirationExceedsMax)
		assert.Contains(t, err.Error(), "90 days")
	})

	t.Run("ZeroConfigEnforcesDefaultMax", func(t *testing.T) {
		store := api_keys.NewMockStore()
		// Config with APIKeyMaxExpirationDays=0 should fall back to default
		cfg := &config.Config{
			APIKeyMaxExpirationDays: 0,
		}
		svc := api_keys.NewServiceWithLogger(store, cfg, serviceTestSubSelector{}, logger.Development())

		// Request 365 days - should fail because default max is 90 days
		expiresIn := 365 * 24 * time.Hour
		result, err := svc.CreateAPIKey(ctx, "alice", []string{"users"}, "Test Key", "", &expiresIn, false, "")

		require.Error(t, err, "should reject expiration exceeding default max (90 days)")
		assert.Nil(t, result)
		require.ErrorIs(t, err, api_keys.ErrExpirationExceedsMax)
	})
}

// ============================================================
// EPHEMERAL KEY EXPIRATION TESTS
// ============================================================

// assertExpirationWithinTolerance verifies that expiresAt is within tolerance of expectedDuration from now.
func assertExpirationWithinTolerance(t *testing.T, expiresAtStr string, expectedDuration time.Duration, now time.Time) {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	require.NoError(t, err)

	expectedExpiry := now.Add(expectedDuration)
	diff := expiresAt.Sub(expectedExpiry).Abs()
	assert.LessOrEqual(t, diff, 5*time.Second,
		"expiration should be ~%v from now, got diff: %v", expectedDuration, diff)
}

func TestEphemeralKeyExpiration(t *testing.T) {
	ctx := context.Background()

	t.Run("DefaultExpirationIsOneHour", func(t *testing.T) {
		svc := api_keys.NewServiceWithLogger(api_keys.NewMockStore(), &config.Config{}, serviceTestSubSelector{}, logger.Development())
		now := time.Now().UTC()

		result, err := svc.CreateAPIKey(ctx, "user", []string{"users"}, "ephemeral-test", "", nil, true, "")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.Ephemeral)
		require.NotNil(t, result.ExpiresAt)
		assertExpirationWithinTolerance(t, *result.ExpiresAt, 1*time.Hour, now)
	})

	t.Run("CustomExpirationWithinLimit", func(t *testing.T) {
		svc := api_keys.NewServiceWithLogger(api_keys.NewMockStore(), &config.Config{}, serviceTestSubSelector{}, logger.Development())
		expiresIn := 30 * time.Minute
		now := time.Now().UTC()

		result, err := svc.CreateAPIKey(ctx, "user", []string{"users"}, "short-lived", "", &expiresIn, true, "")

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.ExpiresAt)
		assertExpirationWithinTolerance(t, *result.ExpiresAt, 30*time.Minute, now)
	})

	t.Run("ExactlyOneHour", func(t *testing.T) {
		svc := api_keys.NewServiceWithLogger(api_keys.NewMockStore(), &config.Config{}, serviceTestSubSelector{}, logger.Development())
		expiresIn := 1 * time.Hour

		result, err := svc.CreateAPIKey(ctx, "user", []string{"users"}, "exactly-one-hour", "", &expiresIn, true, "")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.Ephemeral)
	})

	// Table-driven tests for invalid expiration cases
	invalidExpirationTests := []struct {
		name        string
		expiresIn   time.Duration
		expectedErr error
		errContains string
	}{
		{
			name:        "ExceedsOneHourLimit",
			expiresIn:   2 * time.Hour,
			expectedErr: api_keys.ErrExpirationExceedsMax,
			errContains: "cannot exceed 1 hour",
		},
		{
			name:        "ZeroExpiration",
			expiresIn:   0,
			expectedErr: api_keys.ErrExpirationNotPositive,
			errContains: "must be positive",
		},
		{
			name:        "NegativeExpiration",
			expiresIn:   -1 * time.Hour,
			expectedErr: api_keys.ErrExpirationNotPositive,
			errContains: "must be positive",
		},
	}

	for _, tt := range invalidExpirationTests {
		t.Run(tt.name, func(t *testing.T) {
			svc := api_keys.NewServiceWithLogger(api_keys.NewMockStore(), &config.Config{}, serviceTestSubSelector{}, logger.Development())
			expiresIn := tt.expiresIn

			result, err := svc.CreateAPIKey(ctx, "user", []string{"users"}, "test-key", "", &expiresIn, true, "")

			require.Error(t, err)
			assert.Nil(t, result)
			require.ErrorIs(t, err, tt.expectedErr)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

// subSelectorStub implements api_keys.SubscriptionSelector for CreateAPIKey subscription tests.
type subSelectorStub struct {
	selectErr          error
	highestPriorityErr error
	// highestName is returned by SelectHighestPriority on success; empty defaults to "from-priority".
	highestName string
}

func (s subSelectorStub) Select(_ []string, _ string, requested string, _ string) (*subscription.SelectResponse, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	return &subscription.SelectResponse{Name: requested, Phase: "Active"}, nil
}

func (s subSelectorStub) SelectHighestPriority(_ []string, _ string) (*subscription.SelectResponse, error) {
	if s.highestPriorityErr != nil {
		return nil, s.highestPriorityErr
	}
	name := s.highestName
	if name == "" {
		name = "from-priority"
	}
	return &subscription.SelectResponse{Name: name, Phase: "Active"}, nil
}

func TestCreateAPIKey_Subscription(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	user := "u"
	groups := []string{"g"}

	t.Run("stores_explicit_subscription_name", func(t *testing.T) {
		store := api_keys.NewMockStore()
		svc := api_keys.NewServiceWithLogger(store, cfg, subSelectorStub{}, logger.Development())

		result, err := svc.CreateAPIKey(ctx, user, groups, "key", "", nil, false, "team-a")
		require.NoError(t, err)
		require.Equal(t, "team-a", result.Subscription)

		meta, err := store.Get(ctx, result.ID)
		require.NoError(t, err)
		require.Equal(t, "team-a", meta.Subscription)
	})

	t.Run("defaults_to_highest_priority_when_omitted", func(t *testing.T) {
		store := api_keys.NewMockStore()
		svc := api_keys.NewServiceWithLogger(store, cfg, subSelectorStub{}, logger.Development())

		result, err := svc.CreateAPIKey(ctx, user, groups, "key", "", nil, false, "")
		require.NoError(t, err)
		require.Equal(t, "from-priority", result.Subscription)
	})

	t.Run("selector_errors_do_not_persist_key", func(t *testing.T) {
		errTests := []struct {
			name      string
			stub      subSelectorStub
			requested string
			assertErr func(*testing.T, error)
		}{
			{
				name: "subscription_not_found",
				stub: subSelectorStub{
					selectErr: &subscription.SubscriptionNotFoundError{Subscription: "missing-sub"},
				},
				requested: "missing-sub",
				assertErr: func(t *testing.T, err error) {
					t.Helper()
					var target *subscription.SubscriptionNotFoundError
					require.ErrorAs(t, err, &target)
				},
			},
			{
				name: "subscription_access_denied",
				stub: subSelectorStub{
					selectErr: &subscription.AccessDeniedError{Subscription: "denied-sub"},
				},
				requested: "denied-sub",
				assertErr: func(t *testing.T, err error) {
					t.Helper()
					var target *subscription.AccessDeniedError
					require.ErrorAs(t, err, &target)
				},
			},
			{
				name: "no_accessible_subscription",
				stub: subSelectorStub{
					highestPriorityErr: &subscription.NoSubscriptionError{},
				},
				requested: "",
				assertErr: func(t *testing.T, err error) {
					t.Helper()
					var target *subscription.NoSubscriptionError
					require.ErrorAs(t, err, &target)
				},
			},
		}

		for _, tt := range errTests {
			t.Run(tt.name, func(t *testing.T) {
				store := api_keys.NewMockStore()
				svc := api_keys.NewServiceWithLogger(store, cfg, tt.stub, logger.Development())

				result, err := svc.CreateAPIKey(ctx, user, groups, "key", "", nil, false, tt.requested)
				require.Error(t, err)
				require.Nil(t, result)
				tt.assertErr(t, err)

				res, sErr := store.Search(ctx, user, &api_keys.SearchFilters{}, &api_keys.SortParams{By: api_keys.DefaultSortBy, Order: api_keys.DefaultSortOrder},
					&api_keys.PaginationParams{Limit: 10, Offset: 0})
				require.NoError(t, sErr)
				assert.Empty(t, res.Keys)
			})
		}
	})
}

// ============================================================
// CLEANUP EXPIRED EPHEMERAL KEYS TESTS
// ============================================================

func TestCleanupExpiredEphemeral(t *testing.T) {
	ctx := context.Background()

	t.Run("DeletesExpiredEphemeralKeys", func(t *testing.T) {
		svc, store := createTestService(t)

		// Add active regular key
		err := store.AddKey(ctx, "alice", "regular-1", "hash-1", "Regular", "", nil, "default-sub", nil, false)
		require.NoError(t, err)

		// Add expired ephemeral key
		pastExpiry := time.Now().Add(-1 * time.Hour)
		err = store.AddKey(ctx, "alice", "ephemeral-1", "hash-2", "Ephemeral", "", nil, "default-sub", &pastExpiry, true)
		require.NoError(t, err)

		count, err := svc.CleanupExpiredEphemeral(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)

		// Ephemeral key should be gone
		_, err = store.Get(ctx, "ephemeral-1")
		require.ErrorIs(t, err, api_keys.ErrKeyNotFound)

		// Regular key should still exist
		_, err = store.Get(ctx, "regular-1")
		require.NoError(t, err)
	})

	t.Run("ReturnsZeroWhenNoExpiredKeys", func(t *testing.T) {
		svc, _ := createTestService(t)

		count, err := svc.CleanupExpiredEphemeral(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})
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

func TestCreateAPIKey_ValidatesSubscriptionPhase(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	user := "testuser"
	groups := []string{"g1"}

	tests := []struct {
		name        string
		phase       string
		deleting    bool
		expectError bool
		errorMsg    string
	}{
		{
			name:        "rejects Failed subscription (prevents key spam)",
			phase:       "Failed",
			deleting:    false,
			expectError: true,
			errorMsg:    "Failed phase",
		},
		{
			name:        "allows Pending subscription (enforcement at inference time)",
			phase:       "Pending",
			deleting:    false,
			expectError: false,
		},
		{
			name:        "allows Degraded subscription (enforcement at inference time)",
			phase:       "Degraded",
			deleting:    false,
			expectError: false,
		},
		{
			name:        "rejects unreconciled subscription (empty phase)",
			phase:       "",
			deleting:    false,
			expectError: true,
			errorMsg:    "unreconciled",
		},
		{
			name:        "allows Active subscription",
			phase:       "Active",
			deleting:    false,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock selector that returns subscription with specific health status
			selector := &mockHealthSelector{
				phase:    tt.phase,
				deleting: tt.deleting,
			}

			store := api_keys.NewMockStore()
			svc := api_keys.NewServiceWithLogger(store, cfg, selector, logger.Development())

			_, err := svc.CreateAPIKey(ctx, user, groups, "test-key", "", nil, false, "test-sub")

			if tt.expectError {
				require.Error(t, err, "Expected error for %s", tt.name)
				var modelErr *subscription.ModelUnhealthyError
				require.ErrorAs(t, err, &modelErr, "Expected ModelUnhealthyError")
				require.Contains(t, modelErr.Message, tt.errorMsg, "Error message should contain: %s", tt.errorMsg)
			} else {
				require.NoError(t, err, "Expected no error for %s", tt.name)
			}
		})
	}
}

// mockHealthSelector implements SubscriptionSelector for health testing.
type mockHealthSelector struct {
	phase    string
	deleting bool
}

func (m *mockHealthSelector) Select(_ []string, _ string, _ string, _ string) (*subscription.SelectResponse, error) {
	// Simulate health validation that real selector does for API key creation
	// API key creation path blocks Failed and unreconciled (empty phase)
	if m.phase == "" {
		return nil, &subscription.ModelUnhealthyError{
			Subscription: "test-sub",
			Phase:        "",
			Reason:       "SubscriptionNotReady",
			Message:      "subscription is unreconciled (no status.phase set)",
		}
	}
	if m.phase == "Failed" {
		return nil, &subscription.ModelUnhealthyError{
			Subscription: "test-sub",
			Phase:        "Failed",
			Reason:       "SubscriptionNotReady",
			Message:      "subscription is in Failed phase (cannot create API keys)",
		}
	}

	resp := &subscription.SelectResponse{
		Name:  "test-sub",
		Phase: m.phase,
	}
	if m.deleting {
		resp.DeletionTimestamp = "2026-04-08T12:00:00Z"
	}
	return resp, nil
}

func (m *mockHealthSelector) SelectHighestPriority(_ []string, _ string) (*subscription.SelectResponse, error) {
	//nolint:unqueryvet // False positive - not a SQL query
	return m.Select(nil, "", "", "")
}
