package api_keys //nolint:testpackage // Directly verifies unexported routing state and fake database-driver behavior.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type recordedQuery struct {
	pool  string
	query string
}

type queryRecorder struct {
	mu      sync.Mutex
	queries []recordedQuery
}

func (r *queryRecorder) record(pool, query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, recordedQuery{pool: pool, query: query})
}

func (r *queryRecorder) queriesFor(pool string) []recordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []recordedQuery
	for _, query := range r.queries {
		if query.pool == pool {
			result = append(result, query)
		}
	}
	return result
}

func (r *queryRecorder) count(pool string) int {
	return len(r.queriesFor(pool))
}

type fakeConnector struct {
	pool                   string
	recorder               *queryRecorder
	queryErr               error
	rowsErr                error
	rowsCloseErr           error
	emptyRows              bool
	revokedRows            bool
	blockPingUntilCanceled bool
	cancelDuringQuery      func()
	closed                 bool
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{connector: c}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return fakeDriver{}
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use OpenDB with fakeConnector")
}

type fakeConn struct {
	connector *fakeConnector
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported")
}

func (c *fakeConn) Close() error {
	c.connector.closed = true
	return nil
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *fakeConn) Ping(ctx context.Context) error {
	if c.connector.blockPingUntilCanceled {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.connector.recorder.record(c.connector.pool, query)
	if c.connector.cancelDuringQuery != nil {
		c.connector.cancelDuringQuery()
		return nil, ctx.Err()
	}
	if c.connector.queryErr != nil {
		return nil, c.connector.queryErr
	}
	rows := newFakeRows(query)
	rows.err = c.connector.rowsErr
	rows.closeErr = c.connector.rowsCloseErr
	rows.revoked = c.connector.revokedRows
	if c.connector.emptyRows && strings.Contains(query, "WHERE id = $1") {
		rows.rows = nil
	}
	return rows, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.connector.recorder.record(c.connector.pool, query)
	return driver.RowsAffected(1), nil
}

type fakeRows struct {
	columns  []string
	rows     [][]driver.Value
	next     int
	err      error
	closeErr error
	revoked  bool
}

func newFakeRows(query string) *fakeRows {
	if strings.Contains(query, "pg_last_xact_replay_timestamp") {
		return &fakeRows{columns: []string{"lag"}, rows: [][]driver.Value{{12.5}}}
	}
	if strings.Contains(query, "SELECT COUNT(*)") {
		return &fakeRows{columns: []string{"count"}, rows: [][]driver.Value{{int64(1)}}}
	}
	if strings.Contains(query, "WHERE key_hash = $1") {
		return &fakeRows{
			columns: make([]string, 11),
			rows: [][]driver.Value{{
				"key-id", "user", "key-name", nil, "{}", "subscription", "test-tenant",
				StatusActive, nil, nil, false,
			}},
		}
	}
	if strings.Contains(query, "WHERE id = $1") {
		created := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
		return &fakeRows{
			columns: make([]string, 12),
			rows: [][]driver.Value{{
				"key-id", "key-name", nil, "user", "subscription", "tenant",
				created, nil, StatusActive, nil, false, nil,
			}},
		}
	}
	return &fakeRows{}
}

func (r *fakeRows) Columns() []string {
	return r.columns
}

func (r *fakeRows) Close() error {
	return r.closeErr
}

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.err != nil {
		return r.err
	}
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	if r.revoked {
		dest[7] = StatusRevoked
	}
	r.next++
	return nil
}

func newRoutingStore(t *testing.T, readQueryErr error) (*PostgresStore, *queryRecorder, *fakeConnector, *fakeConnector) {
	t.Helper()

	recorder := &queryRecorder{}
	writeConnector := &fakeConnector{pool: "write", recorder: recorder}
	readConnector := &fakeConnector{pool: "read", recorder: recorder, queryErr: readQueryErr}
	store := NewPostgresStore(sql.OpenDB(writeConnector), logger.Development(), "test-tenant")
	store.readDB = sql.OpenDB(readConnector)
	store.readHealthy.Store(true)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})
	return store, recorder, writeConnector, readConnector
}

func TestPostgresStore_ReadQueriesUseReadPool(t *testing.T) {
	ctx := context.Background()
	store, recorder, _, _ := newRoutingStore(t, nil)

	_, err := store.Search(ctx, "user", "test-tenant", &SearchFilters{},
		&SortParams{By: DefaultSortBy, Order: DefaultSortOrder}, &PaginationParams{Limit: 10, Offset: 0})
	require.NoError(t, err)
	_, err = store.List(ctx, "user", PaginationParams{Limit: 10, Offset: 0}, nil)
	require.NoError(t, err)
	_, err = store.Get(ctx, "key-id")
	require.NoError(t, err)
	key, err := store.GetByHash(ctx, "hash")
	require.NoError(t, err)
	require.NotNil(t, key)

	assert.Equal(t, 4, recorder.count("read"))
	assert.Zero(t, recorder.count("write"))
	assert.True(t, store.readHealthy.Load())
}

func TestPostgresStore_MutationsUseWritePool(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(store *PostgresStore) error
	}{
		{
			name: "add key",
			call: func(store *PostgresStore) error {
				return store.AddKey(ctx, "user", "key-id", "hash", "name", "", nil,
					"subscription", "test-tenant", nil, false, nil)
			},
		},
		{
			name: "bulk revoke dry run",
			call: func(store *PostgresStore) error {
				_, err := store.BulkRevoke(ctx, "user", "", "test-tenant", true)
				return err
			},
		},
		{
			name: "bulk revoke",
			call: func(store *PostgresStore) error {
				_, err := store.BulkRevoke(ctx, "user", "", "test-tenant", false)
				return err
			},
		},
		{
			name: "invalidate tenant",
			call: func(store *PostgresStore) error {
				_, err := store.InvalidateTenant(ctx, "test-tenant")
				return err
			},
		},
		{
			name: "invalidate subscription",
			call: func(store *PostgresStore) error {
				_, err := store.InvalidateSubscription(ctx, "test-tenant", "subscription")
				return err
			},
		},
		{
			name: "revoke",
			call: func(store *PostgresStore) error {
				return store.Revoke(ctx, "key-id")
			},
		},
		{
			name: "update last used",
			call: func(store *PostgresStore) error {
				return store.UpdateLastUsed(ctx, "key-id")
			},
		},
		{
			name: "delete expired ephemeral",
			call: func(store *PostgresStore) error {
				_, err := store.DeleteExpiredEphemeral(ctx)
				return err
			},
		},
		{
			name: "delete soft deleted",
			call: func(store *PostgresStore) error {
				_, err := store.DeleteSoftDeleted(ctx, time.Hour)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, recorder, _, _ := newRoutingStore(t, nil)
			require.NoError(t, test.call(store))
			assert.Equal(t, 1, recorder.count("write"))
			assert.Zero(t, recorder.count("read"))
		})
	}
}

func TestPostgresStore_APIKeyVerificationFallsBackOnce(t *testing.T) {
	ctx := context.Background()
	readErr := errors.New("read pool unavailable")
	store, recorder, _, _ := newRoutingStore(t, readErr)

	key, err := store.GetByHash(ctx, "hash")
	require.NoError(t, err)
	require.NotNil(t, key)

	assert.Equal(t, 1, recorder.count("read"))
	assert.Equal(t, 1, recorder.count("write"))
	assert.Equal(t, uint64(1), store.metrics.fallbacks.Load())
	assert.False(t, store.readHealthy.Load())
}

func TestPostgresStore_ReadFallbackCoversRowConsumptionFailures(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(store *PostgresStore) (any, error)
	}{
		{
			name: "get by hash",
			call: func(store *PostgresStore) (any, error) {
				return store.GetByHash(ctx, "hash")
			},
		},
		{
			name: "search",
			call: func(store *PostgresStore) (any, error) {
				return store.Search(ctx, "user", "test-tenant", &SearchFilters{},
					&SortParams{By: DefaultSortBy, Order: DefaultSortOrder}, &PaginationParams{Limit: 10, Offset: 0})
			},
		},
		{
			name: "list",
			call: func(store *PostgresStore) (any, error) {
				return store.List(ctx, "user", PaginationParams{Limit: 10, Offset: 0}, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, recorder, _, readConnector := newRoutingStore(t, nil)
			readConnector.rowsErr = errors.New("replica disconnected while streaming rows")

			result, err := test.call(store)
			require.NoError(t, err)
			require.NotNil(t, result)

			// A query can succeed at the database/sql boundary and fail later in
			// Next, Scan, or rows.Err. Those failures must still use the documented
			// one-request fallback rather than exposing a replica outage to callers.
			assert.Equal(t, 1, recorder.count("read"))
			assert.Equal(t, 1, recorder.count("write"))
			assert.Equal(t, uint64(1), store.metrics.fallbacks.Load())
			assert.False(t, store.readHealthy.Load())
		})
	}
}

func TestPostgresStore_RequestCancellationDoesNotFallback(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, store *PostgresStore) (any, error)
	}{
		{
			name: "get by hash",
			call: func(ctx context.Context, store *PostgresStore) (any, error) {
				return store.GetByHash(ctx, "hash")
			},
		},
		{
			name: "get",
			call: func(ctx context.Context, store *PostgresStore) (any, error) {
				return store.Get(ctx, "key-id")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			store, recorder, _, readConnector := newRoutingStore(t, nil)
			readConnector.cancelDuringQuery = cancel
			store.readHealthy.Store(false)

			result, err := test.call(ctx, store)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, result)

			// Cancellation belongs to the request, not the replica. It must not trigger
			// a primary retry or change read-pool health while an outage is visible.
			assert.Equal(t, 1, recorder.count("read"))
			assert.Zero(t, recorder.count("write"))
			assert.Zero(t, store.metrics.fallbacks.Load())
			assert.False(t, store.readHealthy.Load())
		})
	}
}

func TestPostgresStore_GetNoRowRestoresReadHealth(t *testing.T) {
	ctx := context.Background()
	store, recorder, _, readConnector := newRoutingStore(t, nil)
	readConnector.emptyRows = true
	store.readHealthy.Store(false)

	_, err := store.Get(ctx, "missing-key")
	require.ErrorIs(t, err, ErrKeyNotFound)

	// QueryRow reports a completed empty result as sql.ErrNoRows during Scan. That is
	// proof the replica served the query, so it must restore operational health even
	// though the caller receives the not-found domain error.
	assert.Equal(t, 1, recorder.count("read"))
	assert.Zero(t, recorder.count("write"))
	assert.True(t, store.readHealthy.Load())
}

func TestPostgresStore_DomainReadResultFallsBackWhenCloseFails(t *testing.T) {
	ctx := context.Background()
	store, recorder, _, readConnector := newRoutingStore(t, nil)
	readConnector.rowsCloseErr = errors.New("replica disconnected while closing rows")
	readConnector.revokedRows = true

	key, err := store.GetByHash(ctx, "hash")
	require.NoError(t, err)
	require.NotNil(t, key)

	// The replica can return a revoked key (a deterministic domain result) and still
	// expose a disconnect while closing the cursor. Close is therefore part of the
	// fallback boundary and must override the domain result's healthy classification.
	assert.Equal(t, 1, recorder.count("read"))
	assert.Equal(t, 1, recorder.count("write"))
	assert.Equal(t, uint64(1), store.metrics.fallbacks.Load())
	assert.False(t, store.readHealthy.Load())
}

func TestPostgresStore_PingsUseSeparateTimeouts(t *testing.T) {
	store, _, writeConnector, _ := newRoutingStore(t, nil)
	writeConnector.blockPingUntilCanceled = true

	store.collectPings()

	// A primary that consumes its full ping timeout must not cancel the replica's
	// independent health-check context and falsely degrade a healthy read pool.
	assert.True(t, store.readHealthy.Load())
	writeFailures, ok := store.metrics.pingFailures.Load("write")
	require.True(t, ok)
	writeFailureCount, ok := writeFailures.(*atomic.Uint64)
	require.True(t, ok)
	assert.Equal(t, uint64(1), writeFailureCount.Load())
	_, ok = store.metrics.pingFailures.Load("read")
	assert.False(t, ok)
}

func TestPostgresStore_RecordQueryConcurrently(t *testing.T) {
	store, recorder, _, _ := newRoutingStore(t, nil)

	const updates = 100
	var workers sync.WaitGroup
	workers.Add(updates)
	for range updates {
		go func() {
			defer workers.Done()
			store.recordQuery("get_by_hash", "read")
		}()
	}
	workers.Wait()

	counterValue, ok := store.metrics.queries.Load(queryMetricKey{operation: "get_by_hash", pool: "read"})
	require.True(t, ok)
	queryCount, ok := counterValue.(*atomic.Uint64)
	require.True(t, ok)
	assert.Equal(t, uint64(updates), queryCount.Load())
	assert.Zero(t, recorder.count("read"))
}

func TestPostgresStore_APIKeyVerificationUsesReplicaSnapshot(t *testing.T) {
	ctx := context.Background()
	store, recorder, _, _ := newRoutingStore(t, nil)

	keyHash := "hash"
	require.NoError(t, store.AddKey(ctx, "user", "key-id", keyHash, "name", "", nil,
		"subscription", "test-tenant", nil, false, nil))
	require.NoError(t, store.Revoke(ctx, "key-id"))

	// The fake read connector intentionally keeps returning the pre-revocation snapshot.
	// This test makes the consistency contract explicit: with a configured replica,
	// validation observes revocation only after replication lag.
	key, err := store.GetByHash(ctx, keyHash)
	require.NoError(t, err)
	require.NotNil(t, key)
	assert.Equal(t, StatusActive, key.Status)
	assert.True(t, store.readHealthy.Load())
	assert.Equal(t, 1, recorder.count("read"))
	assert.Equal(t, 2, recorder.count("write"))
}

func TestPostgresStore_NoReadPoolUsesWritePool(t *testing.T) {
	ctx := context.Background()
	recorder := &queryRecorder{}
	connector := &fakeConnector{pool: "write", recorder: recorder}
	store := NewPostgresStore(sql.OpenDB(connector), logger.Development(), "test-tenant")
	defer func() {
		require.NoError(t, store.Close())
	}()

	_, err := store.Search(ctx, "user", "test-tenant", &SearchFilters{},
		&SortParams{By: DefaultSortBy, Order: DefaultSortOrder}, &PaginationParams{Limit: 10, Offset: 0})
	require.NoError(t, err)

	assert.Equal(t, 1, recorder.count("write"))
	assert.Zero(t, recorder.count("read"))
	readReplicaConfigured, ok := store.DatabaseHealth()["read_replica_configured"].(bool)
	require.True(t, ok)
	assert.False(t, readReplicaConfigured)
}

func TestPostgresStore_CloseClosesBothPools(t *testing.T) {
	ctx := context.Background()
	store, _, writeConnector, readConnector := newRoutingStore(t, nil)

	require.NoError(t, store.writeDB.PingContext(ctx))
	require.NoError(t, store.readDB.PingContext(ctx))
	require.NoError(t, store.Close())

	assert.True(t, writeConnector.closed)
	assert.True(t, readConnector.closed)
}

func TestPostgresStore_MetricsExposeRoutingFallbackAndLag(t *testing.T) {
	store, recorder, _, _ := newRoutingStore(t, nil)
	store.recordReadFallback()
	store.recordQuery("search", "read")
	store.recordPingFailure("read")

	registry := prometheus.NewPedanticRegistry()
	require.NoError(t, registry.Register(store))
	families, err := registry.Gather()
	require.NoError(t, err)

	names := make(map[string]struct{})
	for _, family := range families {
		names[family.GetName()] = struct{}{}
	}
	for _, name := range []string{
		"maas_api_database_queries_total",
		"maas_api_database_read_fallbacks_total",
		"maas_api_database_ping_failures_total",
		"maas_api_database_connections",
		"maas_api_database_replication_lag_seconds",
		"maas_api_database_replication_lag_warning_seconds",
		"maas_api_database_replication_lag_critical_seconds",
	} {
		assert.Contains(t, names, name)
	}
	assert.Equal(t, 1, recorder.count("read"))
}
