// Package api_keys provides API key management with PostgreSQL storage.
// This implementation is designed for production use with golang-migrate
// for schema management and sqlc for type-safe queries.
package api_keys

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// Add stores a legacy API key (for backward compatibility with K8s SA tokens).
func (s *PostgresStore) Add(ctx context.Context, username string, apiKey *APIKey) error {
	if apiKey.JTI == "" {
		return ErrEmptyJTI
	}
	if apiKey.Name == "" {
		return ErrEmptyName
	}

	var createdAt time.Time
	if apiKey.IssuedAt > 0 {
		createdAt = time.Unix(apiKey.IssuedAt, 0).UTC()
	} else {
		createdAt = time.Now().UTC()
	}
	expiresAt := time.Unix(apiKey.ExpiresAt, 0).UTC()

	query := `
		INSERT INTO api_keys (id, username, name, description, key_hash, key_prefix, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, '', '', 'active', $5, $6)
	`
	_, err := s.db.ExecContext(ctx, query, apiKey.JTI, username, apiKey.Name, apiKey.Description, createdAt, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to insert key: %w", err)
	}
	return nil
}

// AddPermanentKey stores an API key with hash-only storage (no plaintext).
func (s *PostgresStore) AddPermanentKey(ctx context.Context, username, keyID, keyHash, keyPrefix, name, description, userGroups string, expiresAt *time.Time) error {
	if keyID == "" {
		return ErrEmptyJTI
	}
	if name == "" {
		return ErrEmptyName
	}
	if keyHash == "" {
		return errors.New("key hash is required")
	}
	if userGroups == "" {
		return errors.New("user groups are required")
	}

	query := `
		INSERT INTO api_keys (id, username, name, description, key_hash, key_prefix, user_groups, status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8, $9)
	`
	_, err := s.db.ExecContext(ctx, query, keyID, username, name, description, keyHash, keyPrefix, userGroups, time.Now().UTC(), expiresAt)
	if err != nil {
		return fmt.Errorf("failed to insert API key: %w", err)
	}

	s.logger.Debug("Stored API key", "id", keyID, "prefix", keyPrefix, "user", username)
	return nil
}

// List returns all API keys for a user.
func (s *PostgresStore) List(ctx context.Context, username string) ([]ApiKeyMetadata, error) {
	query := `
		SELECT id, key_prefix, name, description, created_at, expires_at, status, last_used_at
		FROM api_keys 
		WHERE username = $1
		ORDER BY created_at DESC
	`
	rows, err := s.db.QueryContext(ctx, query, username)
	if err != nil {
		return nil, fmt.Errorf("failed to list keys: %w", err)
	}
	defer rows.Close()

	var keys []ApiKeyMetadata
	for rows.Next() {
		var k ApiKeyMetadata
		var createdAt time.Time
		var expiresAt, lastUsedAt sql.NullTime
		var description sql.NullString

		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Name, &description, &createdAt, &expiresAt, &k.Status, &lastUsedAt); err != nil {
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

	return keys, nil
}

// Get retrieves a single API key by ID.
func (s *PostgresStore) Get(ctx context.Context, keyID string) (*ApiKeyMetadata, error) {
	query := `
		SELECT id, key_prefix, name, description, username, created_at, expires_at, status, last_used_at
		FROM api_keys 
		WHERE id = $1
	`
	row := s.db.QueryRowContext(ctx, query, keyID)

	var k ApiKeyMetadata
	var createdAt time.Time
	var expiresAt, lastUsedAt sql.NullTime
	var description sql.NullString

	if err := row.Scan(&k.ID, &k.KeyPrefix, &k.Name, &description, &k.Username, &createdAt, &expiresAt, &k.Status, &lastUsedAt); err != nil {
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
func (s *PostgresStore) GetByHash(ctx context.Context, keyHash string) (*ApiKeyMetadata, error) {
	query := `
		SELECT id, username, name, description, key_prefix, user_groups, status, expires_at, last_used_at
		FROM api_keys
		WHERE key_hash = $1
	`
	row := s.db.QueryRowContext(ctx, query, keyHash)

	var k ApiKeyMetadata
	var expiresAt, lastUsedAt sql.NullTime
	var description, userGroups sql.NullString

	if err := row.Scan(&k.ID, &k.Username, &k.Name, &description, &k.KeyPrefix, &userGroups, &k.Status, &expiresAt, &lastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("database lookup failed: %w", err)
	}

	if description.Valid {
		k.Description = description.String
	}
	if userGroups.Valid && userGroups.String != "" {
		// Parse JSON array of groups
		var groups []string
		if err := json.Unmarshal([]byte(userGroups.String), &groups); err == nil {
			k.OriginalUserGroups = groups
		}
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = lastUsedAt.Time.UTC().Format(time.RFC3339)
	}

	// Check expiration and auto-update status if expired
	if expiresAt.Valid && time.Now().UTC().After(expiresAt.Time) {
		if k.Status == TokenStatusActive {
			// Auto-update status to expired
			updateQuery := `UPDATE api_keys SET status = 'expired' WHERE id = $1 AND status = 'active'`
			if _, err := s.db.ExecContext(ctx, updateQuery, k.ID); err != nil {
				s.logger.Warn("Failed to update expired key status", "key_id", k.ID, "error", err)
			}
			k.Status = TokenStatusExpired
		}
	}

	// Reject revoked/expired keys
	if k.Status == TokenStatusRevoked || k.Status == TokenStatusExpired {
		return nil, ErrInvalidKey
	}

	return &k, nil
}

// InvalidateAll revokes all active keys for a user.
func (s *PostgresStore) InvalidateAll(ctx context.Context, username string) error {
	query := `UPDATE api_keys SET status = 'revoked' WHERE username = $1 AND status = 'active'`
	result, err := s.db.ExecContext(ctx, query, username)
	if err != nil {
		return fmt.Errorf("failed to revoke keys: %w", err)
	}

	rows, _ := result.RowsAffected()
	s.logger.Info("Revoked all keys for user", "count", rows, "user", username)
	return nil
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
