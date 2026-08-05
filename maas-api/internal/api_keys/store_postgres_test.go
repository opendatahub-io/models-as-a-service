//go:build integration

package api_keys_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

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
	keyHash := uuid.New().String()  // Unique hash to allow multiple runs without dropping table.
	err := store.AddKey(ctx, "alice", keyID, keyHash,
		"legacy-key", "no labels", []string{"group1"},
		"subscription1", "test-tenant", nil, false, nil)
	require.NoError(t, err)

	// Retrieve and verify labels is nil (not empty map)
	key, err := store.Get(ctx, keyID)
	require.NoError(t, err)
	assert.Nil(t, key.Labels) // Important: should be nil, not empty map
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
	t.Helper()
	
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Fatal("TEST_DATABASE_URL environment variable not set")
	}
	
	testLogger := logger.Development()
	store, err := api_keys.NewPostgresStoreFromURL(context.Background(), testLogger, dbURL, "test-tenant")
	if err != nil {
		t.Fatalf("Failed to create PostgreSQL store: %v", err)
	}
	
	return store
}