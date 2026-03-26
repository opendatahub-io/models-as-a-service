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
		return &subscription.SelectResponse{Name: requested}, nil
	}
	return &subscription.SelectResponse{Name: "default-sub"}, nil
}

func (serviceTestSubSelector) SelectHighestPriority(_ []string, _ string) (*subscription.SelectResponse, error) {
	return &subscription.SelectResponse{Name: "default-sub"}, nil
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

	// Create a valid API key
	keyID := "test-key-id"
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
	assert.Equal(t, username, result.UserID)
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
	keyID := "revoked-key-id"
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
	keyID := "expired-key-id"
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
	keyID := "no-groups-key"
	plainKey, hash := createTestAPIKey(t)
	username := "dave"

	err := store.AddKey(ctx, username, keyID, hash, "No Groups Key", "", nil, "default-sub", nil, false)
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
	keyID := "get-test-key"
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
	keyID := "revoke-test-key"
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
	return &subscription.SelectResponse{Name: requested}, nil
}

func (s subSelectorStub) SelectHighestPriority(_ []string, _ string) (*subscription.SelectResponse, error) {
	if s.highestPriorityErr != nil {
		return nil, s.highestPriorityErr
	}
	name := s.highestName
	if name == "" {
		name = "from-priority"
	}
	return &subscription.SelectResponse{Name: name}, nil
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
// TEST HELPERS
// ============================================================

// createTestAPIKey generates a valid API key and its hash for testing.
func createTestAPIKey(t *testing.T) (string, string) {
	t.Helper()
	plainKey, hash, _, err := api_keys.GenerateAPIKey()
	require.NoError(t, err)
	return plainKey, hash
}
