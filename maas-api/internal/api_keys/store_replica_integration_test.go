//go:build integration

package api_keys

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func testDatabaseURLs(t *testing.T) (writeURL, readURL string) {
	t.Helper()

	writeURL = os.Getenv("TEST_DATABASE_URL")
	if writeURL == "" {
		t.Skip("Skipping integration test (TEST_DATABASE_URL not set)")
	}

	// A separate TEST_READ_DATABASE_URL should point to a real replica. Using the
	// writable URL still creates two independent database/sql pools, which is enough
	// to exercise pool selection and fallback with the real PostgreSQL driver.
	readURL = os.Getenv("TEST_READ_DATABASE_URL")
	if readURL == "" {
		readURL = writeURL
	}
	return writeURL, readURL
}

func waitForReadReplication(t *testing.T, store *PostgresStore, keyHash string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Poll the read pool directly, rather than through GetByHash, so this setup
	// synchronization cannot be satisfied by the primary fallback. With a real
	// asynchronous replica, AddKey can commit on the primary before the row is
	// locally visible; asserting immediately would test replication timing rather
	// than whether API-key verification uses the read pool.
	var lastErr error
	for {
		var visible bool
		err := store.readDB.QueryRowContext(
			ctx,
			`SELECT EXISTS(
				SELECT 1 FROM api_keys
				WHERE key_hash = $1 AND tenant = $2 AND deleted_at IS NULL
			)`,
			keyHash,
			store.tenantName,
		).Scan(&visible)
		if err == nil && visible {
			return
		}
		if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				t.Fatalf("timed out waiting for key hash on read replica; last error: %v", lastErr)
			}
			t.Fatalf("timed out waiting for key hash on read replica")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestPostgresStoreFromURLs_APIKeyVerificationUsesReadPool(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	writeURL, readURL := testDatabaseURLs(t)
	store, err := NewPostgresStoreFromURLs(ctx, logger.Development(), writeURL, readURL, "test-tenant")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close())
	}()

	keyHash := uuid.New().String()
	require.NoError(t, store.AddKey(ctx, "integration-user", uuid.New().String(), keyHash,
		"integration-key", "", nil, "integration-subscription", "test-tenant", nil, false, nil))

	waitForReadReplication(t, store, keyHash)

	// Closing only the writable pool proves that the validation lookup does not depend
	// on it when the read pool is healthy. Close is idempotent, so deferred cleanup is safe.
	require.NoError(t, store.writeDB.Close())

	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	require.NotNil(t, key)
	assert.Equal(t, "integration-user", key.Username)
	assert.Equal(t, StatusActive, key.Status)
	assert.True(t, store.readHealthy.Load())
}

func TestPostgresStoreFromURLs_APIKeyVerificationFallsBackToWritePool(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	writeURL, readURL := testDatabaseURLs(t)
	store, err := NewPostgresStoreFromURLs(ctx, logger.Development(), writeURL, readURL, "test-tenant")
	require.NoError(t, err)
	defer func() {
		require.NoError(t, store.Close())
	}()

	keyHash := uuid.New().String()
	require.NoError(t, store.AddKey(ctx, "integration-user", uuid.New().String(), keyHash,
		"integration-key", "", nil, "integration-subscription", "test-tenant", nil, false, nil))

	require.NoError(t, store.readDB.Close())

	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	require.NotNil(t, key)
	assert.Equal(t, "integration-user", key.Username)
	assert.Equal(t, uint64(1), store.metrics.fallbacks.Load())
	assert.False(t, store.readHealthy.Load())
}

func TestPostgresStoreFromURLs_UnreachableReadPoolFailsStartup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	writeURL, _ := testDatabaseURLs(t)
	unreachableReadURL := "postgres://127.0.0.1:1/maas?sslmode=disable&connect_timeout=1"
	_, err := NewPostgresStoreFromURLs(ctx, logger.Development(), writeURL, unreachableReadURL, "test-tenant")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to connect to read-only PostgreSQL")
}
