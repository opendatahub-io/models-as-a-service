//go:build integration

package api_keys_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// TestPostgresStore_LabelsRoundTrip tests storing and retrieving labels.
func TestPostgresStore_LabelsRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	store := setupTestPostgresStore(t)
	defer store.Close()

	labels := map[string]string{
		"cmdb_id":     "AST123456",
		"cost_center": "CC-DATA-001",
		"environment": "production",
	}

	keyID := uuid.New().String()
	err := store.AddKey(ctx, "alice", keyID, "hash123",
		"test-key", "test description", []string{"group1"},
		"subscription1", "test-tenant", nil, false, labels)
	require.NoError(t, err)

	// Retrieve and verify labels
	key, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Equal(t, labels, key.Labels)
}

// TestPostgresStore_SearchByLabels tests JSONB containment queries.
func TestPostgresStore_SearchByLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	store := setupTestPostgresStore(t)
	defer store.Close()

	// Insert keys with different labels
	labels1 := map[string]string{"cmdb_id": "AST123", "env": "prod"}
	labels2 := map[string]string{"cmdb_id": "AST456", "env": "dev"}
	labels3 := map[string]string{"cost_center": "CC-001"}

	require.NoError(t, store.AddKey(ctx, "alice", uuid.New().String(), "hash1",
		"key1", "", []string{}, "sub1", "test-tenant", nil, false, labels1))
	require.NoError(t, store.AddKey(ctx, "alice", uuid.New().String(), "hash2",
		"key2", "", []string{}, "sub1", "test-tenant", nil, false, labels2))
	require.NoError(t, store.AddKey(ctx, "alice", uuid.New().String(), "hash3",
		"key3", "", []string{}, "sub1", "test-tenant", nil, false, labels3))

	// Search by exact match
	result, err := store.Search(ctx, "alice", "test-tenant",
		&api_keys.SearchFilters{LabelsContain: map[string]string{"cmdb_id": "AST123"}},
		&api_keys.SortParams{By: "created_at", Order: "desc"},
		&api_keys.PaginationParams{Limit: 10, Offset: 0})

	require.NoError(t, err)
	assert.Len(t, result.Keys, 1)
	assert.Equal(t, "AST123", result.Keys[0].Labels["cmdb_id"])

	// Search by partial match (JSONB @> supports subset matching)
	result, err = store.Search(ctx, "alice", "test-tenant",
		&api_keys.SearchFilters{LabelsContain: map[string]string{"env": "prod"}},
		&api_keys.SortParams{By: "created_at", Order: "desc"},
		&api_keys.PaginationParams{Limit: 10, Offset: 0})

	require.NoError(t, err)
	assert.Len(t, result.Keys, 1)
	assert.Equal(t, "prod", result.Keys[0].Labels["env"])
}

// TestPostgresStore_BackwardCompatibility_NullLabels tests NULL handling.
func TestPostgresStore_BackwardCompatibility_NullLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	store := setupTestPostgresStore(t)
	defer store.Close()

	// Create key without labels (nil/NULL)
	keyID := uuid.New().String()
	keyHash := uuid.New().String() // Unique hash to allow multiple runs without dropping table.
	err := store.AddKey(ctx, "alice", keyID, keyHash,
		"legacy-key", "no labels", []string{"group1"},
		"subscription1", "test-tenant", nil, false, nil)
	require.NoError(t, err)

	// Retrieve and verify labels is nil (not empty map)
	key, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Nil(t, key.Labels) // Important: should be nil, not empty map
}

func TestAddKeyAndGetByHashStorePlainGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "plain-groups-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()

	userGroups := []string{"team-a", uuid.NewString() + "/admins"}
	keyID := uuid.NewString()
	keyHash := uuid.NewString()
	require.NoError(t, store.AddKey(ctx, "plain-user", keyID, keyHash,
		"plain groups key", "", userGroups, "plain-subscription", tenant, nil, false, nil))

	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	assert.Equal(t, userGroups, key.Groups)
}

// TestPostgresStore_LifecycleInvalidation verifies that lifecycle cleanup
// soft-deletes keys, prevents validation, and preserves subscription scope.
func TestPostgresStore_LifecycleInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "test-tenant-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()

	keyIDs := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	keyHashes := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	subscriptions := []string{"sub-delete", "sub-delete", "sub-keep"}
	for i, id := range keyIDs {
		require.NoError(t, store.AddKey(ctx, "lifecycle-user", id, keyHashes[i],
			"lifecycle-key", "", nil, subscriptions[i],
			tenant, nil, false, nil))
	}

	count, err := store.InvalidateSubscription(ctx, tenant, "sub-delete")
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	retainedID := uuid.NewString()
	retainedHash := uuid.NewString()
	require.NoError(t, store.AddKey(ctx, "lifecycle-user", retainedID, retainedHash,
		"retained-key", "", nil, "sub-keep", tenant, nil, false, nil))
	require.NoError(t, store.Revoke(ctx, retainedID))

	deletedSubscription := "sub-delete"
	singleSubscriptionResult, err := store.Search(
		ctx,
		"lifecycle-user",
		tenant,
		&api_keys.SearchFilters{
			Subscription: &deletedSubscription,
			Status:       []string{string(api_keys.StatusRevoked)},
		},
		&api_keys.SortParams{By: "created_at", Order: "desc"},
		&api_keys.PaginationParams{Limit: 10, Offset: 0},
	)
	require.NoError(t, err)
	assert.Empty(t, singleSubscriptionResult.Keys)

	subscriptionNamesResult, err := store.Search(
		ctx,
		"lifecycle-user",
		tenant,
		&api_keys.SearchFilters{
			SubscriptionNames: []string{"sub-delete", "sub-keep"},
			Status:            []string{string(api_keys.StatusRevoked)},
		},
		&api_keys.SortParams{By: "created_at", Order: "desc"},
		&api_keys.PaginationParams{Limit: 10, Offset: 0},
	)
	require.NoError(t, err)
	require.Len(t, subscriptionNamesResult.Keys, 1)
	assert.Equal(t, retainedID, subscriptionNamesResult.Keys[0].ID)

	for i, id := range keyIDs[:2] {
		key, err := store.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, api_keys.StatusRevoked, key.Status)
		_, err = store.GetByHash(ctx, keyHashes[i])
		assert.ErrorIs(t, err, api_keys.ErrKeyNotFound)
	}
	kept, err := store.Get(ctx, keyIDs[2])
	require.NoError(t, err)
	assert.Equal(t, api_keys.StatusActive, kept.Status)

	count, err = store.InvalidateTenant(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "tenant cleanup should only newly invalidate remaining live rows")

	// Move one soft-deleted row beyond retention and verify physical cleanup is
	// still restricted to this store's tenant.
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, "UPDATE api_keys SET deleted_at = NOW() - INTERVAL '91 days' WHERE id = $1", keyIDs[0])
	require.NoError(t, err)
	deleted, err := store.DeleteSoftDeleted(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1))
}

func TestPostgresStore_SubscriptionIdentityRotationRevokesKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()

	observerID := uuid.NewString()
	session, err := store.BeginSubscriptionObservation(ctx, observerID, tenant)
	require.NoError(t, err)
	require.NoError(t, session.Heartbeat(ctx))
	identityID, err := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "identity-sub",
		ObjectUID: "uid-1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, identityID)
	require.NoError(t, session.Commit())

	keyID := uuid.NewString()
	keyHash := uuid.NewString()
	require.NoError(t, store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", keyID, keyHash, "identity key", "", []string{"users"},
		"identity-sub", identityID, "subscriptions", tenant, nil, false, nil, time.Minute,
	))

	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	assert.Equal(t, identityID, key.SubscriptionIdentityID)
	assert.Equal(t, "subscriptions", key.SubscriptionNamespace)

	rotationSession, err := store.BeginSubscriptionObservation(ctx, observerID, tenant)
	require.NoError(t, err)
	require.NoError(t, rotationSession.Heartbeat(ctx))
	_, err = rotationSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "identity-sub",
		ObjectUID: "uid-2",
	})
	require.ErrorIs(t, err, api_keys.ErrSubscriptionIdentityPending)
	require.NoError(t, rotationSession.Commit())

	_, err = store.GetByHash(ctx, keyHash)
	require.ErrorIs(t, err, api_keys.ErrInvalidKey)

	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, `
		UPDATE subscription_identities
		SET rotation_pending_at = NOW() - INTERVAL '2 seconds'
		WHERE id = $1
	`, identityID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = NOW() - INTERVAL '2 seconds',
		    observed_at = NOW() - INTERVAL '2 seconds'
		WHERE subscription_id = $1
	`, identityID)
	require.NoError(t, err)

	recoverySession, err := store.BeginSubscriptionObservation(ctx, observerID, tenant)
	require.NoError(t, err)
	replacementID, err := recoverySession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "identity-sub",
		ObjectUID: "uid-2",
		Settle:    time.Second,
	})
	require.NoError(t, err)
	require.NotEqual(t, identityID, replacementID)
	require.NoError(t, recoverySession.Commit())

	replacementKeyID := uuid.NewString()
	replacementKeyHash := uuid.NewString()
	require.NoError(t, store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", replacementKeyID, replacementKeyHash, "replacement identity key", "", []string{"users"},
		"identity-sub", replacementID, "subscriptions", tenant, nil, false, nil, time.Minute,
	))
	replacementKey, err := store.GetByHash(ctx, replacementKeyHash)
	require.NoError(t, err)
	assert.Equal(t, replacementID, replacementKey.SubscriptionIdentityID)
}

func TestPostgresStore_RecordAbsenceAndCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-cleanup-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	observerOne := uuid.NewString()
	observerTwo := uuid.NewString()
	session, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, session.Heartbeat(ctx))
	identityID, err := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "cleanup-sub",
		ObjectUID: "uid-1",
	})
	require.NoError(t, err)
	require.NoError(t, session.Commit())

	err = store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", uuid.NewString(), uuid.NewString(), "wrong identity", "", []string{"users"},
		"cleanup-sub", uuid.NewString(), "subscriptions", tenant, nil, false, nil, time.Minute,
	)
	require.ErrorIs(t, err, api_keys.ErrSubscriptionIdentityMismatch)

	keyID := uuid.NewString()
	keyHash := uuid.NewString()
	require.NoError(t, store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", keyID, keyHash, "identity key", "", []string{"users"},
		"cleanup-sub", identityID, "subscriptions", tenant, nil, false, nil, time.Minute,
	))

	peerSession, err := store.BeginSubscriptionObservation(ctx, observerTwo, tenant)
	require.NoError(t, err)
	require.NoError(t, peerSession.Heartbeat(ctx))
	_, err = peerSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace:        "subscriptions",
		Name:             "cleanup-sub",
		ObjectUID:        "uid-1",
		StatusIdentityID: identityID,
	})
	require.NoError(t, err)
	require.NoError(t, peerSession.Commit())

	absenceSession, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, absenceSession.Heartbeat(ctx))
	result, revoked, err := absenceSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "cleanup-sub",
			ObjectUID:              "uid-1",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupRetained, result)
	assert.Zero(t, revoked)
	require.NoError(t, absenceSession.Commit())

	peerAbsenceSession, err := store.BeginSubscriptionObservation(ctx, observerTwo, tenant)
	require.NoError(t, err)
	require.NoError(t, peerAbsenceSession.Heartbeat(ctx))
	result, revoked, err = peerAbsenceSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "cleanup-sub",
			ObjectUID:              "uid-1",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupBlockedBarrier, result)
	assert.Zero(t, revoked)
	require.NoError(t, peerAbsenceSession.Commit())

	_, err = db.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = NOW() - INTERVAL '2 hours', observed_at = NOW()
		WHERE subscription_id = $1
	`, identityID)
	require.NoError(t, err)

	finalSession, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, finalSession.Heartbeat(ctx))
	result, revoked, err = finalSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "cleanup-sub",
			ObjectUID:              "uid-1",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupCleaned, result)
	assert.Equal(t, 1, revoked)
	require.NoError(t, finalSession.Commit())

	_, err = store.GetByHash(ctx, keyHash)
	require.ErrorIs(t, err, api_keys.ErrInvalidKey)

	replacementSession, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, replacementSession.Heartbeat(ctx))
	replacementID, err := replacementSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "cleanup-sub",
		ObjectUID: "uid-2",
	})
	require.NoError(t, err)
	require.NotEqual(t, identityID, replacementID)
	require.NoError(t, replacementSession.Commit())

	replacementKeyHash := uuid.NewString()
	require.NoError(t, store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", uuid.NewString(), replacementKeyHash, "replacement key", "", []string{"users"},
		"cleanup-sub", replacementID, "subscriptions", tenant, nil, false, nil, time.Minute,
	))
	replacementKey, err := store.GetByHash(ctx, replacementKeyHash)
	require.NoError(t, err)
	assert.Equal(t, replacementID, replacementKey.SubscriptionIdentityID)
}

func TestPostgresStore_RecordAbsenceAndCleanupIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-idempotent-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	neverObservedSession, err := store.BeginSubscriptionObservation(ctx, uuid.NewString(), tenant)
	require.NoError(t, err)
	require.NoError(t, neverObservedSession.Heartbeat(ctx))
	result, revoked, err := neverObservedSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace: "subscriptions",
			Name:      "never-observed-sub",
			ObjectUID: "never-observed-uid",
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupCleaned, result)
	assert.Zero(t, revoked)
	require.NoError(t, neverObservedSession.Commit())

	ownerSession, err := store.BeginSubscriptionObservation(ctx, uuid.NewString(), tenant)
	require.NoError(t, err)
	require.NoError(t, ownerSession.Heartbeat(ctx))
	identityID, err := ownerSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "status-without-observation-sub",
		ObjectUID: "status-uid",
	})
	require.NoError(t, err)
	require.NoError(t, ownerSession.MarkAbsent(ctx, identityID))
	require.NoError(t, ownerSession.Commit())

	unverifiedSession, err := store.BeginSubscriptionObservation(ctx, uuid.NewString(), tenant)
	require.NoError(t, err)
	require.NoError(t, unverifiedSession.Heartbeat(ctx))
	result, revoked, err = unverifiedSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace: "subscriptions",
			Name:      "status-without-observation-sub",
			ObjectUID: "status-uid",
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupBlockedUnverified, result)
	assert.Zero(t, revoked)
	require.NoError(t, unverifiedSession.Commit())

	absenceObserver := uuid.NewString()
	absenceSession, err := store.BeginSubscriptionObservation(ctx, absenceObserver, tenant)
	require.NoError(t, err)
	require.NoError(t, absenceSession.Heartbeat(ctx))
	result, revoked, err = absenceSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "status-without-observation-sub",
			ObjectUID:              "status-uid",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupBlockedBarrier, result)
	assert.Zero(t, revoked)
	require.NoError(t, absenceSession.Commit())

	_, err = db.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = NOW() - INTERVAL '2 hours', observed_at = NOW()
		WHERE subscription_id = $1
	`, identityID)
	require.NoError(t, err)

	finalSession, err := store.BeginSubscriptionObservation(ctx, absenceObserver, tenant)
	require.NoError(t, err)
	require.NoError(t, finalSession.Heartbeat(ctx))
	result, revoked, err = finalSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "status-without-observation-sub",
			ObjectUID:              "status-uid",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupCleaned, result)
	assert.Zero(t, revoked)
	require.NoError(t, finalSession.Commit())

	retrySession, err := store.BeginSubscriptionObservation(ctx, absenceObserver, tenant)
	require.NoError(t, err)
	require.NoError(t, retrySession.Heartbeat(ctx))
	result, revoked, err = retrySession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "status-without-observation-sub",
			ObjectUID:              "status-uid",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupCleaned, result)
	assert.Zero(t, revoked)
	require.NoError(t, retrySession.Commit())
}

func TestPostgresStore_RecordAbsenceBlocksForStaleAndEmptyObservers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-blocked-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	currentObserver := uuid.NewString()
	staleObserver := uuid.NewString()
	session, err := store.BeginSubscriptionObservation(ctx, currentObserver, tenant)
	require.NoError(t, err)
	require.NoError(t, session.Heartbeat(ctx))
	identityID, err := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "blocked-sub",
		ObjectUID: "uid-1",
	})
	require.NoError(t, err)
	require.NoError(t, session.Commit())

	_, err = db.ExecContext(ctx, `
		INSERT INTO subscription_observers (observer_id, tenant, observed_at)
		VALUES ($1, $2, NOW() - INTERVAL '1 hour')
	`, staleObserver, tenant)
	require.NoError(t, err)

	staleBlockSession, err := store.BeginSubscriptionObservation(ctx, currentObserver, tenant)
	require.NoError(t, err)
	require.NoError(t, staleBlockSession.Heartbeat(ctx))
	result, _, err := staleBlockSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "blocked-sub",
			ObjectUID:              "uid-1",
			SubscriptionIdentityID: identityID,
		},
		time.Second,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupBlockedStale, result)
	require.NoError(t, staleBlockSession.Commit())

	_, err = db.ExecContext(ctx, `
		UPDATE subscription_observers
		SET retired_at = NOW()
		WHERE observer_id IN ($1, $2)
	`, currentObserver, staleObserver)
	require.NoError(t, err)

	emptyBlockSession, err := store.BeginSubscriptionObservation(ctx, currentObserver, tenant)
	require.NoError(t, err)
	result, _, err = emptyBlockSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "blocked-sub",
			ObjectUID:              "uid-1",
			SubscriptionIdentityID: identityID,
		},
		time.Second,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupBlockedEmpty, result)
	require.NoError(t, emptyBlockSession.Commit())
}

func TestPostgresStore_TombstonedIdentityDoesNotPoisonReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-replacement-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	observerOne := uuid.NewString()
	observerTwo := uuid.NewString()
	sessionOne, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, sessionOne.Heartbeat(ctx))
	identityID, err := sessionOne.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "replacement-sub",
		ObjectUID: "old-uid",
	})
	require.NoError(t, err)
	require.NoError(t, sessionOne.Commit())

	sessionTwo, err := store.BeginSubscriptionObservation(ctx, observerTwo, tenant)
	require.NoError(t, err)
	require.NoError(t, sessionTwo.Heartbeat(ctx))
	_, err = sessionTwo.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace:        "subscriptions",
		Name:             "replacement-sub",
		ObjectUID:        "old-uid",
		StatusIdentityID: identityID,
	})
	require.NoError(t, err)
	require.NoError(t, sessionTwo.Commit())

	_, err = db.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = NOW() - INTERVAL '2 hours', observed_at = NOW()
		WHERE subscription_id = $1
	`, identityID)
	require.NoError(t, err)

	cleanupSession, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, cleanupSession.Heartbeat(ctx))
	result, _, err := cleanupSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "replacement-sub",
			ObjectUID:              "old-uid",
			SubscriptionIdentityID: identityID,
		},
		time.Hour,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupCleaned, result)
	require.NoError(t, cleanupSession.Commit())

	replacementSession, err := store.BeginSubscriptionObservation(ctx, observerOne, tenant)
	require.NoError(t, err)
	require.NoError(t, replacementSession.Heartbeat(ctx))
	replacementID, err := replacementSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "replacement-sub",
		ObjectUID: "new-uid",
	})
	require.NoError(t, err)
	require.NotEqual(t, identityID, replacementID)
	require.NoError(t, replacementSession.Commit())

	peerReplacementSession, err := store.BeginSubscriptionObservation(ctx, observerTwo, tenant)
	require.NoError(t, err)
	require.NoError(t, peerReplacementSession.Heartbeat(ctx))
	peerReplacementID, err := peerReplacementSession.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "replacement-sub",
		ObjectUID: "new-uid",
	})
	require.NoError(t, err)
	assert.Equal(t, replacementID, peerReplacementID)
	require.NoError(t, peerReplacementSession.Commit())
}

func TestPostgresStore_RecordAbsenceRetainsAPIKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenant := "identity-retained-" + uuid.NewString()
	store := setupTestPostgresStoreForTenant(t, tenant)
	defer store.Close()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	observerID := uuid.NewString()
	session, err := store.BeginSubscriptionObservation(ctx, observerID, tenant)
	require.NoError(t, err)
	require.NoError(t, session.Heartbeat(ctx))
	identityID, err := session.ResolveSubscriptionIdentity(ctx, api_keys.ResolveSubscriptionIdentityRequest{
		Namespace: "subscriptions",
		Name:      "retained-sub",
		ObjectUID: "retained-uid",
	})
	require.NoError(t, err)
	require.NoError(t, session.Commit())

	keyHash := uuid.NewString()
	require.NoError(t, store.AddKeyWithSubscriptionIdentity(
		ctx, "alice", uuid.NewString(), keyHash, "retained key", "", []string{"users"},
		"retained-sub", identityID, "subscriptions", tenant, nil, false, nil, time.Minute,
	))

	absenceSession, err := store.BeginSubscriptionObservation(ctx, observerID, tenant)
	require.NoError(t, err)
	require.NoError(t, absenceSession.Heartbeat(ctx))
	result, revoked, err := absenceSession.RecordAbsenceAndCleanup(
		ctx,
		api_keys.SubscriptionAbsenceRequest{
			Namespace:              "subscriptions",
			Name:                   "retained-sub",
			ObjectUID:              "retained-uid",
			SubscriptionIdentityID: identityID,
			RetainAPIKeys:          true,
		},
		time.Nanosecond,
		time.Minute,
	)
	require.NoError(t, err)
	assert.Equal(t, api_keys.SubscriptionCleanupRetainedPolicy, result)
	assert.Zero(t, revoked)
	require.NoError(t, absenceSession.Commit())

	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	assert.Equal(t, identityID, key.SubscriptionIdentityID)
	var deletedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(
		ctx, "SELECT deleted_at FROM subscription_identities WHERE id = $1", identityID,
	).Scan(&deletedAt))
	assert.False(t, deletedAt.Valid)
}

func TestPostgresStore_ObserverTenantIsImmutable(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	ctx := context.Background()
	tenantA := "observer-tenant-a-" + uuid.NewString()
	tenantB := "observer-tenant-b-" + uuid.NewString()
	observerID := uuid.NewString()

	storeA := setupTestPostgresStoreForTenant(t, tenantA)
	defer storeA.Close()
	sessionA, err := storeA.BeginSubscriptionObservation(ctx, observerID, tenantA)
	require.NoError(t, err)
	require.NoError(t, sessionA.Heartbeat(ctx))
	require.NoError(t, sessionA.Commit())

	storeB := setupTestPostgresStoreForTenant(t, tenantB)
	defer storeB.Close()
	sessionB, err := storeB.BeginSubscriptionObservation(ctx, observerID, tenantB)
	require.NoError(t, err)
	err = sessionB.Heartbeat(ctx)
	require.ErrorIs(t, err, api_keys.ErrTenantMismatch)
	require.NoError(t, sessionB.Rollback())
}

// TestPostgresStore_ConcurrentIndexesCreated verifies that all indexes registered
// in concurrentMigrations are created during store initialization.
func TestPostgresStore_ConcurrentIndexesCreated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	// Ensure migrations have run by initializing the store.
	store := setupTestPostgresStore(t)
	defer store.Close()

	// Use a separate connection to query pg_indexes directly,
	// avoiding the need to expose *sql.DB on PostgresStore.
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	// The db_driver.go file contains a list of concurrent migrations that are applied to the database.
	// This test verifies that all of these indexes are created in the database.
	for _, name := range api_keys.ConcurrentMigrationNames() {
		t.Run(name, func(t *testing.T) {
			var indexName string
			err := db.QueryRow(
				"SELECT indexname FROM pg_indexes WHERE tablename = 'api_keys' AND indexname = $1",
				name,
			).Scan(&indexName)

			require.NoError(t, err, "expected index %q to exist", name)
			assert.Equal(t, name, indexName)
		})
	}
}

// setupTestPostgresStore creates a PostgreSQL store for testing.
// Requires TEST_DATABASE_URL environment variable (e.g., "postgres://user:pass@localhost:5432/testdb").
func setupTestPostgresStore(t *testing.T) *api_keys.PostgresStore {
	return setupTestPostgresStoreForTenant(t, "test-tenant")
}

func setupTestPostgresStoreForTenant(t *testing.T, tenant string) *api_keys.PostgresStore {
	t.Helper()

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Fatal("TEST_DATABASE_URL environment variable not set")
	}

	testLogger := logger.Development()
	store, err := api_keys.NewPostgresStoreFromURL(context.Background(), testLogger, dbURL, tenant)
	if err != nil {
		t.Fatalf("Failed to create PostgreSQL store: %v", err)
	}

	return store
}
