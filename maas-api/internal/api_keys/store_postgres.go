// Package api_keys provides API key management with PostgreSQL storage.
// This implementation uses hand-written SQL with parameterized queries for safety.
// Schema is managed by golang-migrate (see db/schema).
package api_keys

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lib/pq"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// PostgresStore implements MetadataStore using PostgreSQL.
// It expects the schema to be managed by golang-migrate (see db/schema).
type PostgresStore struct {
	// writeDB is the authority for schema state and mutations. It is also the fallback
	// for reads when no replica is configured or the replica fails.
	writeDB *sql.DB

	// readDB is the preferred pool for the API-key validation hot path and administrative
	// reads. It aliases writeDB when no read URL is configured, preserving the older
	// deployments' strict read-from-primary behavior.
	readDB     *sql.DB
	logger     *logger.Logger
	tenantName string // Tenant identifier for filtering queries

	// readHealthy is operational state, not a routing policy. It is cleared by read-query
	// failures and restored only by a successful direct read. A successful fallback does
	// not clear it because operators need visibility into repeated replica issues.
	readHealthy atomic.Bool
	metrics     *storeMetrics
}

// Compile-time check that PostgresStore implements MetadataStore.
var _ MetadataStore = (*PostgresStore)(nil)

// NewPostgresStore creates a new PostgreSQL-backed store.
// The database connection and schema migration should be handled by the db package.
// tenantName is used to filter all database queries to enforce tenant isolation.
func NewPostgresStore(db *sql.DB, log *logger.Logger, tenantName string) *PostgresStore {
	store := &PostgresStore{
		writeDB:    db,
		readDB:     db,
		logger:     log,
		tenantName: tenantName,
	}
	store.readHealthy.Store(true)
	store.metrics = newStoreMetrics()
	return store
}

func (s *PostgresStore) hasReadReplica() bool {
	return s.readDB != nil && s.readDB != s.writeDB
}

// readWithFallback routes a local read and applies the one-request fallback policy. It
// is used by the API-key validation hot path and administrative reads, but never by a
// mutation. Callers that require read-your-write semantics must not use this helper.
//
// A single retry bounds the additional primary load during a replica outage. The retry
// is per request rather than a process-wide circuit breaker: database/sql already owns
// connection pooling, and fallback metrics plus logs provide the rollout signal without
// adding hidden routing state.
//
// Not-found results are deliberately final. In particular, a key can be committed by
// AddKey on the primary and remain absent from a lagging replica; retrying that result
// on the primary would mask replica divergence and make routing semantics depend on
// when a key was created.
func readWithFallback[T any](
	ctx context.Context,
	store *PostgresStore,
	operation string,
	run func(context.Context, *sql.DB) (T, error),
) (T, error) {
	if !store.hasReadReplica() {
		store.recordQuery(operation, "write")
		return run(ctx, store.readDB)
	}

	store.recordQuery(operation, "read")
	result, err := run(ctx, store.readDB)
	if err == nil || isDomainReadResult(err) {
		// A domain result means database/sql successfully returned and consumed the
		// replica rows. That includes deterministic no-row and invalid-key results,
		// which must not be retried as though they were infrastructure failures.
		store.readHealthy.Store(true)
		return result, err
	}
	if ctx.Err() != nil {
		// Request cancellation or expiration is not a replica-health signal. Preserve
		// the original result and error so a canceled request cannot add primary load,
		// increment fallback metrics, or change the read-pool health state.
		return result, err
	}

	store.readHealthy.Store(false)
	store.recordReadFallback()
	store.recordQuery(operation, "write")
	store.logger.Warn("PostgreSQL read query failed; falling back to writable primary",
		"operation", operation, "error", err)
	return run(ctx, store.writeDB)
}

func isDomainReadResult(err error) bool {
	return errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrInvalidKey)
}

// queryReadRowsWithFallback executes a query and consumes its rows inside the fallback
// boundary. This is important because database/sql can return *sql.Rows successfully and
// still surface a connection failure later through Scan, Next, or rows.Err.
func queryReadRowsWithFallback[T any](
	ctx context.Context,
	store *PostgresStore,
	operation string,
	query string,
	consumeRows func(*sql.Rows) (T, error),
	args ...any,
) (T, error) {
	// The callback must complete row iteration inside this function. Keeping Scan,
	// Next, and rows.Err inside the fallback boundary is essential: database/sql can
	// accept QueryContext and later expose a replica disconnect while streaming rows.
	run := func(ctx context.Context, db *sql.DB) (T, error) {
		rows, queryErr := db.QueryContext(ctx, query, args...)
		if queryErr != nil {
			var zero T
			return zero, queryErr
		}
		var result T
		var consumeErr error
		// The deferred no-op close protects against a callback panic. The explicit close
		// below is part of consuming database/sql rows: a callback can return a
		// deterministic domain error after scanning its final row, while the
		// driver discovers a replica disconnect only when the rows cursor is closed.
		// database/sql makes Rows.Close idempotent, so the deferred call is a no-op on
		// normal returns. Override the callback result on a close failure so a domain
		// error cannot bypass fallback and falsely mark an unhealthy replica healthy.
		defer func() { _ = rows.Close() }()
		result, consumeErr = consumeRows(rows)
		if iterationErr := rows.Err(); iterationErr != nil {
			var zero T
			result = zero
			consumeErr = iterationErr
		}
		if closeErr := rows.Close(); closeErr != nil {
			var zero T
			result = zero
			consumeErr = closeErr
		}
		return result, consumeErr
	}
	return readWithFallback(ctx, store, operation, run)
}

// AddKey stores an API key with hash-only storage (no plaintext).
// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
// ephemeral marks the key as short-lived for programmatic use.
//
// Note on naming: keyID parameter is the database UUID/JTI (primary key), distinct from
// the embedded_key_id salt in the API key format. The keyHash is computed as
// SHA-256(embedded_key_id + "\x00" + secret), where embedded_key_id is encoded in the
// API key string (sk-oai-{embedded_key_id}_{secret}).
//
// Note: keyPrefix is NOT stored (security - reduces brute-force attack surface).
// labels are stored as JSONB (NULL if empty) for efficient lookups and filtering.
// AddKey commits to the writable primary. When a read replica is configured, GetByHash
// may not see the new key until replication catches up; that not-found result is not
// retried on the primary because doing so would hide replica divergence.
func (s *PostgresStore) AddKey(
	ctx context.Context, username, keyID, keyHash, name, description string, userGroups []string, subscription string, tenant string, expiresAt *time.Time, ephemeral bool,
	labels map[string]string,
) error {
	if keyID == "" {
		return ErrEmptyJTI
	}
	if name == "" {
		return ErrEmptyName
	}
	if keyHash == "" {
		return errors.New("key hash is required")
	}
	if subscription == "" {
		return errors.New("subscription is required")
	}
	if ephemeral && expiresAt == nil {
		return errors.New("ephemeral keys must have an expiration time")
	}
	// Enforce tenant isolation - only allow creating keys for this store's tenant
	if tenant != s.tenantName {
		return fmt.Errorf("tenant mismatch: attempted to create key for tenant %q but store is scoped to %q", tenant, s.tenantName)
	}
	if userGroups == nil {
		userGroups = []string{}
	}

	// Marshal labels to JSONB (NULL if empty)
	var labelsJSON []byte
	var err error
	if len(labels) > 0 {
		labelsJSON, err = json.Marshal(labels)
		if err != nil {
			return fmt.Errorf("failed to marshal labels: %w", err)
		}
	}

	query := `
		INSERT INTO api_keys (id, username, name, description, key_hash, user_groups, subscription, tenant, status, created_at, expires_at, ephemeral, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, $10, $11, $12)
	`

	var labelsParam any
	if labelsJSON != nil {
		labelsParam = labelsJSON
	} else {
		labelsParam = nil
	}

	s.recordQuery("add_key", "write")
	// Use pq.Array to handle PostgreSQL TEXT[] type
	_, err = s.writeDB.ExecContext(ctx, query,
		keyID, username, name, description, keyHash, pq.Array(userGroups),
		subscription, tenant, time.Now().UTC(), expiresAt, ephemeral, labelsParam)
	if err != nil {
		return fmt.Errorf("failed to insert API key: %w", err)
	}

	s.logger.Debug("Stored API key", "id", keyID, "user", logger.RedactValue(username), "ephemeral", ephemeral)
	return nil
}

// List returns a paginated list of API keys with optional filtering.
// Pagination is mandatory - no unbounded queries allowed.
// Fetches limit+1 items to efficiently determine if more pages exist.
// username can be empty (admin viewing all users) or specific username.
// statuses can filter by status (active, revoked, expired) - empty means all statuses.
// Note: Ephemeral keys are excluded by default (use Search with IncludeEphemeral for full control).
func (s *PostgresStore) List(ctx context.Context, username string, params PaginationParams, statuses []string) (*PaginatedResult, error) {
	// Validate params
	if params.Limit < 1 || params.Limit > 100 {
		return nil, errors.New("limit must be between 1 and 100")
	}
	if params.Offset < 0 {
		return nil, errors.New("offset must be non-negative")
	}

	// Build WHERE clause dynamically
	var whereClauses []string
	var args []any
	argPos := 1

	// Filter by tenant for multi-tenancy isolation
	whereClauses = append(whereClauses, fmt.Sprintf("tenant = $%d", argPos))
	args = append(args, s.tenantName)
	argPos++
	// Exclude ephemeral keys by default
	whereClauses = append(whereClauses, "ephemeral = FALSE")

	if username != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("username = $%d", argPos))
		args = append(args, username)
		argPos++
	}

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, status := range statuses {
			placeholders[i] = fmt.Sprintf("$%d", argPos)
			args = append(args, strings.TrimSpace(status))
			argPos++
		}
		whereClauses = append(whereClauses, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ",")))
	}

	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Fetch limit+1 to determine hasMore
	fetchLimit := params.Limit + 1

	query := fmt.Sprintf(`
		SELECT id, name, description, subscription, tenant, created_at, expires_at, status, last_used_at, ephemeral
		FROM api_keys
		%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argPos, argPos+1)

	args = append(args, fetchLimit, params.Offset)

	result, err := queryReadRowsWithFallback(
		ctx,
		s,
		"list",
		query,
		func(rows *sql.Rows) (*PaginatedResult, error) {
			var keys []ApiKey
			for rows.Next() {
				var k ApiKey
				var createdAt time.Time
				var expiresAt, lastUsedAt sql.NullTime
				var description sql.NullString

				if err := rows.Scan(&k.ID, &k.Name, &description, &k.Subscription, &k.Tenant, &createdAt, &expiresAt, &k.Status, &lastUsedAt, &k.Ephemeral); err != nil {
					return nil, fmt.Errorf("failed to scan row: %w", err)
				}

				k.CreationDate = createdAt.UTC().Format(time.RFC3339)
				if description.Valid {
					k.Description = description.String
				}
				if expiresAt.Valid {
					k.ExpirationDate = expiresAt.Time.UTC().Format(time.RFC3339)
				}
				if lastUsedAt.Valid {
					k.LastUsedAt = lastUsedAt.Time.UTC().Format(time.RFC3339)
				}

				keys = append(keys, k)
			}

			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("row iteration error: %w", err)
			}

			// Determine if there are more results
			hasMore := len(keys) > params.Limit
			if hasMore {
				// Trim to requested limit
				keys = keys[:params.Limit]
			}

			return &PaginatedResult{
				Keys:    keys,
				HasMore: hasMore,
			}, nil
		},
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}
	return result, nil
}

// Search implements flexible API key search with filtering, sorting, pagination.
// Ephemeral keys are excluded by default unless IncludeEphemeral filter is set to true.
func (s *PostgresStore) Search(
	ctx context.Context,
	username string,
	tenant string,
	filters *SearchFilters,
	sort *SortParams,
	pagination *PaginationParams,
) (*PaginatedResult, error) {
	// Validate pagination
	if pagination.Limit < 1 || pagination.Limit > MaxLimit {
		return nil, errors.New("limit must be between 1 and 100")
	}
	if pagination.Offset < 0 {
		return nil, errors.New("offset must be non-negative")
	}

	// Build WHERE clause
	var whereClauses []string
	var args []any
	argPos := 1

	// Tenant scoping is mandatory - use store's tenant for isolation
	whereClauses = append(whereClauses, fmt.Sprintf("tenant = $%d", argPos))
	args = append(args, s.tenantName)
	argPos++
	// Exclude ephemeral keys by default
	if filters.IncludeEphemeral == nil || !*filters.IncludeEphemeral {
		whereClauses = append(whereClauses, "ephemeral = FALSE")
	}

	// Filter by username
	if username != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("username = $%d", argPos))
		args = append(args, username)
		argPos++
	}

	// Filter by effective status (accounts for expired keys still stored as 'active')
	// Use CASE expression to compute effective status at query time
	effectiveStatusExpr := "CASE WHEN status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW() THEN 'expired' ELSE status END"
	if len(filters.Status) > 0 {
		placeholders := make([]string, len(filters.Status))
		for i, status := range filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argPos)
			args = append(args, strings.TrimSpace(status))
			argPos++
		}
		whereClauses = append(whereClauses, fmt.Sprintf("(%s) IN (%s)", effectiveStatusExpr, strings.Join(placeholders, ",")))
	}

	// Filter by subscription name
	if filters.Subscription != nil && *filters.Subscription != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("subscription = $%d", argPos))
		args = append(args, strings.TrimSpace(*filters.Subscription))
		argPos++
	}

	// Add labels containment filter
	if len(filters.LabelsContain) > 0 {
		// Convert filter to JSONB for @> containment operator
		filterJSON, err := json.Marshal(filters.LabelsContain)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal labels filter: %w", err)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("labels @> $%d::jsonb", argPos))
		args = append(args, filterJSON)
		argPos++
	}

	// Build final WHERE clause
	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Enforce sort defaults and allowlist at the store boundary, independent of
	// handler validation, to prevent SQL injection from any direct caller.
	sortBy := sort.By
	if !ValidSortFields[sortBy] {
		sortBy = DefaultSortBy
	}
	sortOrder := strings.ToLower(sort.Order)
	if !ValidSortOrders[sortOrder] {
		sortOrder = DefaultSortOrder
	}

	// Build ORDER BY clause
	orderByClause := fmt.Sprintf("ORDER BY %s %s", sortBy, strings.ToUpper(sortOrder))

	// Handle NULL values for nullable timestamp columns (NULLS LAST)
	if sortBy == "expires_at" || sortBy == "last_used_at" {
		if sortOrder == "asc" {
			orderByClause = fmt.Sprintf("ORDER BY %s ASC NULLS LAST", sortBy)
		} else {
			orderByClause = fmt.Sprintf("ORDER BY %s DESC NULLS LAST", sortBy)
		}
	}

	// Fetch one extra to determine hasMore
	fetchLimit := pagination.Limit + 1

	// Use effective status in SELECT to match WHERE clause filtering
	effectiveStatusSelect := "CASE WHEN status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW() THEN 'expired' ELSE status END"

	query := fmt.Sprintf(`
		SELECT id, name, description, subscription, tenant, username, created_at, expires_at, %s AS status, last_used_at, ephemeral, labels
		FROM api_keys
		%s
		%s
		LIMIT $%d OFFSET $%d
	`, effectiveStatusSelect, whereClause, orderByClause, argPos, argPos+1)

	args = append(args, fetchLimit, pagination.Offset)

	result, err := queryReadRowsWithFallback(ctx, s, "search", query, func(rows *sql.Rows) (*PaginatedResult, error) {
		var keys []ApiKey
		for rows.Next() {
			var key ApiKey
			var createdAt, expiresAt, lastUsedAt sql.NullTime
			var description sql.NullString
			var labelsJSON []byte

			err := rows.Scan(
				&key.ID,
				&key.Name,
				&description,
				&key.Subscription,
				&key.Tenant,
				&key.Username,
				&createdAt,
				&expiresAt,
				&key.Status,
				&lastUsedAt,
				&key.Ephemeral,
				&labelsJSON,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to scan API key: %w", err)
			}

			// Convert timestamps
			if createdAt.Valid {
				key.CreationDate = createdAt.Time.Format(time.RFC3339)
			}
			if description.Valid {
				key.Description = description.String
			}
			if expiresAt.Valid {
				key.ExpirationDate = expiresAt.Time.Format(time.RFC3339)
			}
			if lastUsedAt.Valid {
				key.LastUsedAt = lastUsedAt.Time.Format(time.RFC3339)
			}

			// Parse labels JSONB
			if labelsJSON != nil {
				if err := json.Unmarshal(labelsJSON, &key.Labels); err != nil {
					s.logger.Warn("Failed to unmarshal labels", "keyId", key.ID, "error", err)
					key.Labels = nil // Set to nil on unmarshal error (defensive)
				}
			}

			keys = append(keys, key)
		}

		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("error iterating API keys: %w", err)
		}

		// Check for more results
		hasMore := len(keys) > pagination.Limit
		if hasMore {
			keys = keys[:pagination.Limit]
		}

		return &PaginatedResult{
			Keys:    keys,
			HasMore: hasMore,
		}, nil
	}, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search API keys: %w", err)
	}
	return result, nil
}

// Get retrieves a single API key by ID.
func (s *PostgresStore) Get(ctx context.Context, keyID string) (*ApiKey, error) {
	// Use effective status to return 'expired' for keys past expiration date
	query := `
		SELECT id, name, description, username, subscription, tenant, created_at, expires_at,
			CASE WHEN status = 'active' AND expires_at IS NOT NULL AND expires_at < NOW() THEN 'expired' ELSE status END AS status,
			last_used_at, ephemeral, labels
		FROM api_keys
		WHERE id = $1 AND tenant = $2
	`
	readPool := "write"
	if s.hasReadReplica() {
		readPool = "read"
	}
	s.recordQuery("get", readPool)
	row := s.readDB.QueryRowContext(ctx, query, keyID, s.tenantName)

	var k ApiKey
	var createdAt time.Time
	var expiresAt, lastUsedAt sql.NullTime
	var description sql.NullString
	var labelsJSON []byte

	err := row.Scan(&k.ID, &k.Name, &description, &k.Username, &k.Subscription, &k.Tenant, &createdAt, &expiresAt, &k.Status, &lastUsedAt, &k.Ephemeral, &labelsJSON)
	fallbackUsed := false
	if err != nil && errors.Is(err, sql.ErrNoRows) {
		// QueryRow surfaces a completed empty result as sql.ErrNoRows during Scan.
		// This is a successful replica query, not divergence or an infrastructure
		// failure, so it must restore health before the not-found domain result returns.
		if s.hasReadReplica() {
			s.readHealthy.Store(true)
		}
		return nil, ErrKeyNotFound
	}
	// Other scan errors are retried once because QueryRow defers connection and query
	// failures until Scan. The no-row case has already returned above as a domain
	// result, so it cannot hide replica divergence behind a primary retry.
	if err != nil && s.hasReadReplica() {
		if ctx.Err() != nil {
			// A canceled or expired request is not evidence that the replica failed.
			// Return the original scan result and error without health or routing changes.
			return nil, err
		}
		s.readHealthy.Store(false)
		s.recordReadFallback()
		s.recordQuery("get", "write")
		s.logger.Warn("PostgreSQL read query failed; falling back to writable primary",
			"operation", "get", "error", err)
		row = s.writeDB.QueryRowContext(ctx, query, keyID, s.tenantName)
		err = row.Scan(&k.ID, &k.Name, &description, &k.Username, &k.Subscription, &k.Tenant, &createdAt, &expiresAt, &k.Status, &lastUsedAt, &k.Ephemeral, &labelsJSON)
		fallbackUsed = true
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("failed to get key: %w", err)
	}
	// A successful direct read proves that the replica is reachable. A successful
	// fallback proves only that the primary works, so the read pool remains degraded.
	if err == nil && s.hasReadReplica() && !fallbackUsed {
		s.readHealthy.Store(true)
	}

	k.CreationDate = createdAt.UTC().Format(time.RFC3339)
	if description.Valid {
		k.Description = description.String
	}
	if expiresAt.Valid {
		k.ExpirationDate = expiresAt.Time.UTC().Format(time.RFC3339)
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = lastUsedAt.Time.UTC().Format(time.RFC3339)
	}

	// Parse labels JSONB
	if labelsJSON != nil {
		if err := json.Unmarshal(labelsJSON, &k.Labels); err != nil {
			s.logger.Warn("Failed to unmarshal labels", "keyId", keyID, "error", err)
			k.Labels = nil // Set to a safe value (empty map) on unmarshal error (defensive)
		}
	}

	return &k, nil
}

// GetByHash looks up an API key by its SHA-256 hash (critical path for validation).
//
// The lookup uses the cluster-local read replica when one is configured. This is the
// primary latency optimization for multi-cluster traffic: the synchronous request no
// longer crosses clusters for its indexed hash lookup. Revocation is therefore bounded
// by replication lag rather than being visible on the next primary read. Deployments
// that require strict immediate revocation must omit DB_READ_CONNECTION_URL.
//
// If the replica is unavailable, the query falls back once to the writable primary.
// Expiration state transitions remain writes and always use the primary.
func (s *PostgresStore) GetByHash(ctx context.Context, keyHash string) (*ApiKey, error) {
	query := `
		SELECT id, username, name, description, user_groups, subscription, tenant, status, expires_at, last_used_at, ephemeral
		FROM api_keys
		WHERE key_hash = $1 AND tenant = $2 AND deleted_at IS NULL
	`
	key, err := queryReadRowsWithFallback(ctx, s, "get_by_hash", query, func(rows *sql.Rows) (*ApiKey, error) {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("database lookup failed: %w", err)
			}
			return nil, ErrKeyNotFound
		}

		var k ApiKey
		var expiresAt, lastUsedAt sql.NullTime
		var description sql.NullString
		var userGroups []string

		// Use pq.Array to scan PostgreSQL TEXT[] into []string
		if err := rows.Scan(&k.ID, &k.Username, &k.Name, &description, pq.Array(&userGroups), &k.Subscription, &k.Tenant, &k.Status, &expiresAt, &lastUsedAt, &k.Ephemeral); err != nil {
			return nil, fmt.Errorf("database lookup failed: %w", err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("database lookup failed: %w", err)
		}

		if description.Valid {
			k.Description = description.String
		}
		// user_groups is now directly scanned as []string - no JSON parsing needed
		k.Groups = userGroups

		if lastUsedAt.Valid {
			k.LastUsedAt = lastUsedAt.Time.UTC().Format(time.RFC3339)
		}

		// Check expiration and auto-update status if expired
		if expiresAt.Valid && time.Now().UTC().After(expiresAt.Time) {
			if k.Status == StatusActive {
				// Auto-update status to expired
				updateQuery := `UPDATE api_keys SET status = 'expired' WHERE id = $1 AND tenant = $2 AND status = 'active'`
				if _, err := s.writeDB.ExecContext(ctx, updateQuery, k.ID, s.tenantName); err != nil {
					s.logger.Warn("Failed to update expired key status", "key_id", k.ID, "error", err)
				}
				k.Status = StatusExpired
			}
		}

		// Reject revoked/expired keys
		if k.Status == StatusRevoked || k.Status == StatusExpired {
			return nil, ErrInvalidKey
		}

		return &k, nil
	}, keyHash, s.tenantName)
	if err != nil {
		return nil, fmt.Errorf("database lookup failed: %w", err)
	}
	return key, nil
}

// bulkRevokeWhereClause builds a parameterized WHERE clause for bulk revoke
// operations. It dynamically adds username and/or subscription filters alongside
// the mandatory tenant and status=active predicates.
func (s *PostgresStore) bulkRevokeWhereClause(username, subscription string) (string, []any) {
	conditions := []string{"tenant = $1", "status = 'active'", "deleted_at IS NULL", "(expires_at IS NULL OR expires_at > NOW())"}
	args := []any{s.tenantName}
	paramIdx := 2

	if username != "" {
		conditions = append(conditions, fmt.Sprintf("username = $%d", paramIdx))
		args = append(args, username)
		paramIdx++
	}
	if subscription != "" {
		conditions = append(conditions, fmt.Sprintf("subscription = $%d", paramIdx))
		args = append(args, subscription)
	}

	return strings.Join(conditions, " AND "), args
}

// BulkRevoke handles both actual revocation and dry-run counting. When dryRun
// is false, it marks all active, non-expired keys matching the scope as revoked
// in a single atomic UPDATE and returns the affected count. When dryRun is true,
// it returns the count without mutating any data.
//
// Both modes use writeDB. Reading the dry-run count from a replica could report stale
// rows, making the preview disagree with the subsequent mutation. Keeping the complete
// method on one pool also prevents a replica outage from changing bulk-revoke semantics.
func (s *PostgresStore) BulkRevoke(ctx context.Context, username, subscription, tenant string, dryRun bool) (int, error) {
	if username == "" && subscription == "" {
		return 0, errors.New("at least one of username or subscription is required")
	}
	if tenant != s.tenantName {
		return 0, fmt.Errorf("%w: attempted to revoke keys for tenant %q but store is scoped to %q", ErrTenantMismatch, tenant, s.tenantName)
	}

	where, args := s.bulkRevokeWhereClause(username, subscription)
	s.recordQuery("bulk_revoke", "write")

	if dryRun {
		//nolint:gosec // where clause uses static column names with parameterized $N placeholders
		query := fmt.Sprintf("SELECT COUNT(*) FROM api_keys WHERE %s", where)

		var count int
		if err := s.writeDB.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return 0, fmt.Errorf("dry-run count query failed: %w", err)
		}
		return count, nil
	}

	//nolint:gosec // where clause uses static column names with parameterized $N placeholders
	query := fmt.Sprintf("UPDATE api_keys SET status = 'revoked' WHERE %s", where)

	result, err := s.writeDB.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to bulk revoke keys: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	count := int(rows)
	s.logger.Info("Bulk revoked keys",
		"count", count,
		"user", logger.RedactValue(username),
		"subscription", logger.RedactValue(subscription),
	)
	return count, nil
}

// InvalidateTenant soft-deletes all keys within this tenant and revokes active keys.
// Returns the count of rows newly soft-deleted.
func (s *PostgresStore) InvalidateTenant(ctx context.Context, tenant string) (int, error) {
	if tenant != s.tenantName {
		return 0, fmt.Errorf("%w: attempted to revoke keys for tenant %q but store is scoped to %q", ErrTenantMismatch, tenant, s.tenantName)
	}

	query := `UPDATE api_keys
		SET deleted_at = NOW(),
		    status = CASE WHEN status = 'active' THEN 'revoked' ELSE status END
		WHERE tenant = $1 AND deleted_at IS NULL`

	s.recordQuery("invalidate_tenant", "write")
	result, err := s.writeDB.ExecContext(ctx, query, s.tenantName)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke tenant keys: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	count := int(rows)
	s.logger.Info("Invalidated all keys for tenant", "count", count, "tenant", s.tenantName)
	return count, nil
}

// InvalidateSubscription soft-deletes all keys for one subscription within a tenant.
func (s *PostgresStore) InvalidateSubscription(ctx context.Context, tenant, subscription string) (int, error) {
	if tenant != s.tenantName {
		return 0, fmt.Errorf("%w: attempted to revoke keys for tenant %q but store is scoped to %q", ErrTenantMismatch, tenant, s.tenantName)
	}
	if strings.TrimSpace(subscription) == "" {
		return 0, errors.New("subscription is required")
	}

	query := `UPDATE api_keys
		SET deleted_at = NOW(),
		    status = CASE WHEN status = 'active' THEN 'revoked' ELSE status END
		WHERE tenant = $1 AND subscription = $2 AND deleted_at IS NULL`
	s.recordQuery("invalidate_subscription", "write")
	result, err := s.writeDB.ExecContext(ctx, query, tenant, subscription)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke subscription keys: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	count := int(rows)
	s.logger.Info("Invalidated all keys for subscription", "count", count,
		"tenant", s.tenantName, "subscription", logger.RedactValue(subscription))
	return count, nil
}

// Revoke marks a specific API key as revoked.
func (s *PostgresStore) Revoke(ctx context.Context, keyID string) error {
	query := `UPDATE api_keys SET status = 'revoked' WHERE id = $1 AND tenant = $2 AND status = 'active'`
	s.recordQuery("revoke", "write")
	result, err := s.writeDB.ExecContext(ctx, query, keyID, s.tenantName)
	if err != nil {
		return fmt.Errorf("failed to revoke key: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return ErrKeyNotFound
	}

	s.logger.Info("Revoked API key", "id", keyID)
	return nil
}

// UpdateLastUsed updates the last_used_at timestamp.
func (s *PostgresStore) UpdateLastUsed(ctx context.Context, keyID string) error {
	query := `UPDATE api_keys SET last_used_at = $1 WHERE id = $2 AND tenant = $3`
	s.recordQuery("update_last_used", "write")
	_, err := s.writeDB.ExecContext(ctx, query, time.Now().UTC(), keyID, s.tenantName)
	if err != nil {
		return fmt.Errorf("failed to update last_used_at: %w", err)
	}
	return nil
}

// DeleteExpiredEphemeral removes expired ephemeral API keys that have been expired for at least 30 minutes.
// The grace period provides a safety net before hard-deleting keys from the database.
// Uses the partial index idx_api_keys_ephemeral_expired for efficient lookups.
// Only deletes keys belonging to this tenant.
func (s *PostgresStore) DeleteExpiredEphemeral(ctx context.Context) (int64, error) {
	query := `DELETE FROM api_keys WHERE tenant = $1 AND deleted_at IS NULL AND ephemeral = TRUE AND expires_at IS NOT NULL AND expires_at < NOW() - INTERVAL '30 minutes'`

	s.recordQuery("delete_expired_ephemeral", "write")
	result, err := s.writeDB.ExecContext(ctx, query, s.tenantName)
	if err != nil {
		return 0, fmt.Errorf("failed to delete expired ephemeral keys: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	if rows > 0 {
		s.logger.Info("Deleted expired ephemeral keys", "count", rows)
	}

	return rows, nil
}

// DeleteSoftDeleted physically removes lifecycle-invalidated keys after retention.
func (s *PostgresStore) DeleteSoftDeleted(ctx context.Context, retention time.Duration) (int64, error) {
	if retention < 0 {
		return 0, errors.New("retention must not be negative")
	}
	query := `DELETE FROM api_keys
		WHERE tenant = $1 AND deleted_at IS NOT NULL AND deleted_at < $2`
	cutoff := time.Now().UTC().Add(-retention)
	s.recordQuery("delete_soft_deleted", "write")
	result, err := s.writeDB.ExecContext(ctx, query, s.tenantName, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete soft-deleted keys: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}
	if rows > 0 {
		s.logger.Info("Deleted retained API keys", "count", rows, "retention", retention.String())
	}
	return rows, nil
}

// Close closes the database connection.
// This should be called during graceful shutdown to prevent connection leaks.
func (s *PostgresStore) Close() error {
	// Both handles are closed independently. When no read URL is configured they are the
	// same handle, and database/sql Close is idempotent. The explicit alias check keeps a
	// future constructor change from leaking a separately allocated read pool.
	var writeErr error
	if s.writeDB != nil {
		writeErr = s.writeDB.Close()
	}
	if s.readDB != nil && s.readDB != s.writeDB {
		if readErr := s.readDB.Close(); writeErr == nil {
			writeErr = readErr
		}
	}
	return writeErr
}
