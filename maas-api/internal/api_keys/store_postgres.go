// Package api_keys provides API key management with PostgreSQL storage.
// This implementation uses hand-written SQL with parameterized queries for safety.
// Schema is managed by golang-migrate (see db/schema).
package api_keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// PostgresStore implements MetadataStore using PostgreSQL.
// It expects the schema to be managed by golang-migrate (see db/schema).
type PostgresStore struct {
	db     *sql.DB
	logger *logger.Logger
}

// Compile-time check that PostgresStore implements MetadataStore.
var _ MetadataStore = (*PostgresStore)(nil)

// NewPostgresStore creates a new PostgreSQL-backed store.
// The database connection and schema migration should be handled by the db package.
func NewPostgresStore(db *sql.DB, log *logger.Logger) *PostgresStore {
	return &PostgresStore{
		db:     db,
		logger: log,
	}
}

// AddKey stores an API key with hash-only storage (no plaintext).
// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
// Note: keyPrefix is NOT stored (security - reduces brute-force attack surface).
func (s *PostgresStore) AddKey(ctx context.Context, username, keyID, keyHash, name, description string, userGroups []string, expiresAt *time.Time) error {
	if keyID == "" {
		return ErrEmptyJTI
	}
	if name == "" {
		return ErrEmptyName
	}
	if keyHash == "" {
		return errors.New("key hash is required")
	}
	if userGroups == nil {
		userGroups = []string{}
	}

	query := `
		INSERT INTO api_keys (id, username, name, description, key_hash, user_groups, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8)
	`
	// Use pq.Array to handle PostgreSQL TEXT[] type
	_, err := s.db.ExecContext(ctx, query, keyID, username, name, description, keyHash, pq.Array(userGroups), time.Now().UTC(), expiresAt)
	if err != nil {
		return fmt.Errorf("failed to insert API key: %w", err)
	}

	s.logger.Debug("Stored API key", "id", keyID, "user", username)
	return nil
}

// List returns a paginated list of API keys with optional filtering.
// Pagination is mandatory - no unbounded queries allowed.
// Fetches limit+1 items to efficiently determine if more pages exist.
// username can be empty (admin viewing all users) or specific username.
// statuses can filter by status (active, revoked, expired) - empty means all statuses.
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

	//nolint:gosec // Dynamic WHERE clause is safe - uses parameterized queries
	query := fmt.Sprintf(`
		SELECT id, name, description, created_at, expires_at, status, last_used_at
		FROM api_keys
		%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d
	`, whereClause, argPos, argPos+1)

	args = append(args, fetchLimit, params.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var k ApiKey
		var createdAt time.Time
		var expiresAt, lastUsedAt sql.NullTime
		var description sql.NullString

		if err := rows.Scan(&k.ID, &k.Name, &description, &createdAt, &expiresAt, &k.Status, &lastUsedAt); err != nil {
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
}

// Search implements flexible API key search with filtering, sorting, pagination.
func (s *PostgresStore) Search(
	ctx context.Context,
	username string,
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

	// Filter by username
	if username != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("username = $%d", argPos))
		args = append(args, username)
		argPos++
	}

	// Filter by status
	if len(filters.Status) > 0 {
		placeholders := make([]string, len(filters.Status))
		for i, status := range filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argPos)
			args = append(args, strings.TrimSpace(status))
			argPos++
		}
		whereClauses = append(whereClauses, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ",")))
	}

	// Build final WHERE clause
	whereClause := ""
	if len(whereClauses) > 0 {
		whereClause = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Build ORDER BY clause
	orderByClause := fmt.Sprintf("ORDER BY %s %s", sort.By, strings.ToUpper(sort.Order))

	// Handle NULL values for nullable timestamp columns (NULLS LAST)
	if sort.By == "expires_at" || sort.By == "last_used_at" {
		if sort.Order == "asc" {
			orderByClause = fmt.Sprintf("ORDER BY %s ASC NULLS LAST", sort.By)
		} else {
			orderByClause = fmt.Sprintf("ORDER BY %s DESC NULLS LAST", sort.By)
		}
	}

	// Fetch one extra to determine hasMore
	fetchLimit := pagination.Limit + 1

	//nolint:gosec // Dynamic ORDER BY is safe - sort.By/Order validated against allowlist in handler
	query := fmt.Sprintf(`
		SELECT id, name, description, created_at, expires_at, status, last_used_at
		FROM api_keys
		%s
		%s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderByClause, argPos, argPos+1)

	args = append(args, fetchLimit, pagination.Offset)

	// Execute query
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search API keys: %w", err)
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var key ApiKey
		var createdAt, expiresAt, lastUsedAt sql.NullTime
		var description sql.NullString

		err := rows.Scan(
			&key.ID,
			&key.Name,
			&description,
			&createdAt,
			&expiresAt,
			&key.Status,
			&lastUsedAt,
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
}

// Get retrieves a single API key by ID.
func (s *PostgresStore) Get(ctx context.Context, keyID string) (*ApiKey, error) {
	query := `
		SELECT id, name, description, username, created_at, expires_at, status, last_used_at
		FROM api_keys
		WHERE id = $1
	`
	row := s.db.QueryRowContext(ctx, query, keyID)

	var k ApiKey
	var createdAt time.Time
	var expiresAt, lastUsedAt sql.NullTime
	var description sql.NullString

	if err := row.Scan(&k.ID, &k.Name, &description, &k.Username, &createdAt, &expiresAt, &k.Status, &lastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("failed to get key: %w", err)
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

	return &k, nil
}

// GetByHash looks up an API key by its SHA-256 hash (critical path for validation).
func (s *PostgresStore) GetByHash(ctx context.Context, keyHash string) (*ApiKey, error) {
	query := `
		SELECT id, username, name, description, user_groups, status, expires_at, last_used_at
		FROM api_keys
		WHERE key_hash = $1
	`
	row := s.db.QueryRowContext(ctx, query, keyHash)

	var k ApiKey
	var expiresAt, lastUsedAt sql.NullTime
	var description sql.NullString
	var userGroups []string

	// Use pq.Array to scan PostgreSQL TEXT[] into []string
	if err := row.Scan(&k.ID, &k.Username, &k.Name, &description, pq.Array(&userGroups), &k.Status, &expiresAt, &lastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrKeyNotFound
		}
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
			updateQuery := `UPDATE api_keys SET status = 'expired' WHERE id = $1 AND status = 'active'`
			if _, err := s.db.ExecContext(ctx, updateQuery, k.ID); err != nil {
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
}

// InvalidateAll revokes all active keys for a user.
// Returns the count of keys that were revoked.
func (s *PostgresStore) InvalidateAll(ctx context.Context, username string) (int, error) {
	query := `UPDATE api_keys SET status = 'revoked' WHERE username = $1 AND status = 'active'`

	result, err := s.db.ExecContext(ctx, query, username)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke keys: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	count := int(rows)
	s.logger.Info("Revoked all keys for user", "count", count, "user", username)
	return count, nil
}

// Revoke marks a specific API key as revoked.
func (s *PostgresStore) Revoke(ctx context.Context, keyID string) error {
	query := `UPDATE api_keys SET status = 'revoked' WHERE id = $1 AND status = 'active'`
	result, err := s.db.ExecContext(ctx, query, keyID)
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
	query := `UPDATE api_keys SET last_used_at = $1 WHERE id = $2`
	_, err := s.db.ExecContext(ctx, query, time.Now().UTC(), keyID)
	if err != nil {
		return fmt.Errorf("failed to update last_used_at: %w", err)
	}
	return nil
}

// Close closes the database connection.
// This should be called during graceful shutdown to prevent connection leaks.
func (s *PostgresStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
