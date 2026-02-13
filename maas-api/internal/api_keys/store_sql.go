package api_keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

var (
	ErrEmptyJTI  = errors.New("token JTI is required and cannot be empty")
	ErrEmptyName = errors.New("token name is required and cannot be empty")
)

type SQLStore struct {
	db     *sql.DB
	dbType DBType
	logger *logger.Logger
}

var _ MetadataStore = (*SQLStore)(nil)

// NewSQLiteStore creates a SQLite store with a file path.
// Use ":memory:" for an in-memory database (ephemeral, for testing).
// Use a file path like "/data/maas-api.db" for persistent storage.
func NewSQLiteStore(ctx context.Context, log *logger.Logger, dbPath string) (*SQLStore, error) {
	if dbPath == "" {
		dbPath = sqliteMemory
	}

	dsn := dbPath
	if dbPath != sqliteMemory {
		dsn = fmt.Sprintf("%s?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000", dbPath)
	}

	db, err := sql.Open(driverSQLite, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	configureConnectionPool(db, DBTypeSQLite)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping SQLite database: %w", err)
	}

	s := &SQLStore{db: db, dbType: DBTypeSQLite, logger: log}
	if err := s.initSchema(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	if dbPath == sqliteMemory {
		log.Info("Connected to SQLite in-memory database (ephemeral - data will be lost on restart)")
	} else {
		log.Info("Connected to SQLite database", "path", dbPath)
	}
	return s, nil
}

func (s *SQLStore) Close() error {
	return s.db.Close()
}

func (s *SQLStore) initSchema(ctx context.Context) error {
	// Use TEXT for timestamps - works for both SQLite and PostgreSQL
	// SQLite doesn't have TIMESTAMPTZ, and TEXT is portable
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS tokens (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT,
		creation_date TEXT NOT NULL,
		expiration_date TEXT NOT NULL
	)`

	if _, err := s.db.ExecContext(ctx, createTableQuery); err != nil {
		return fmt.Errorf("failed to create table: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_tokens_username ON tokens(username)`); err != nil {
		return fmt.Errorf("failed to create username index: %w", err)
	}

	// Migration: Add columns for API keys (sk-oai-* format)
	// These columns support hash-only storage per Feature Refinement "Hash-Only Storage"
	if err := s.migrateForPermanentKeys(ctx); err != nil {
		return fmt.Errorf("failed to migrate for permanent keys: %w", err)
	}

	return nil
}

// migrateForPermanentKeys adds columns needed for API key support (sk-oai-* format)
func (s *SQLStore) migrateForPermanentKeys(ctx context.Context) error {
	// Add key_hash column (SHA-256 hash of the plaintext key)
	// Per Feature Refinement "Hash-Only Storage": plaintext is NEVER stored
	if err := s.addColumnIfNotExists(ctx, "tokens", "key_hash", "TEXT"); err != nil {
		return fmt.Errorf("failed to add key_hash column: %w", err)
	}

	// Add key_prefix column (display prefix like "sk-oai-abc123...")
	if err := s.addColumnIfNotExists(ctx, "tokens", "key_prefix", "TEXT"); err != nil {
		return fmt.Errorf("failed to add key_prefix column: %w", err)
	}

	// Add status column for soft-delete (active, expired, revoked)
	if err := s.addColumnIfNotExists(ctx, "tokens", "status", "TEXT DEFAULT 'active'"); err != nil {
		return fmt.Errorf("failed to add status column: %w", err)
	}

	// Create unique index on key_hash for fast validation lookups
	// This is CRITICAL for performance - Authorino calls validation on every request
	indexQuery := `CREATE UNIQUE INDEX IF NOT EXISTS idx_tokens_key_hash ON tokens(key_hash) WHERE key_hash IS NOT NULL`
	if _, err := s.db.ExecContext(ctx, indexQuery); err != nil {
		return fmt.Errorf("failed to create key_hash index: %w", err)
	}

	return nil
}

// addColumnIfNotExists safely adds a column if it doesn't exist
// Works for both SQLite and PostgreSQL
func (s *SQLStore) addColumnIfNotExists(ctx context.Context, table, column, columnDef string) error {
	// Check if column exists by querying table info
	var exists bool
	var err error

	if s.dbType == DBTypeSQLite {
		// SQLite: use pragma table_info
		query := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = '%s'", table, column)
		err = s.db.QueryRowContext(ctx, query).Scan(&exists)
	} else {
		// PostgreSQL: query information_schema
		query := `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns 
			WHERE table_name = $1 AND column_name = $2
		)`
		err = s.db.QueryRowContext(ctx, query, table, column).Scan(&exists)
	}

	if err != nil {
		return fmt.Errorf("failed to check if column %s exists: %w", column, err)
	}

	if exists {
		return nil // Column already exists
	}

	// Add the column
	alterQuery := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, columnDef)
	if _, err := s.db.ExecContext(ctx, alterQuery); err != nil {
		return fmt.Errorf("failed to add column %s: %w", column, err)
	}

	s.logger.Info("Added column to schema", "table", table, "column", column)
	return nil
}

// placeholder returns the appropriate placeholder for the database type.
// SQLite uses ?, PostgreSQL uses $1, $2, etc.
func (s *SQLStore) placeholder(index int) string {
	return placeholder(s.dbType, index)
}

func (s *SQLStore) Add(ctx context.Context, username string, apiKey *APIKey) error {
	jti := strings.TrimSpace(apiKey.JTI)
	if jti == "" {
		return ErrEmptyJTI
	}

	name := strings.TrimSpace(apiKey.Name)
	if name == "" {
		return ErrEmptyName
	}

	var creationDate time.Time
	if apiKey.IssuedAt > 0 {
		creationDate = time.Unix(apiKey.IssuedAt, 0)
	} else {
		creationDate = time.Now()
	}
	expirationDate := time.Unix(apiKey.ExpiresAt, 0)

	// Store as RFC3339 strings for portability
	creationStr := creationDate.UTC().Format(time.RFC3339)
	expirationStr := expirationDate.UTC().Format(time.RFC3339)

	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`
	INSERT INTO tokens (id, username, name, description, creation_date, expiration_date)
	VALUES (%s, %s, %s, %s, %s, %s)
	`, s.placeholder(1), s.placeholder(2), s.placeholder(3), s.placeholder(4), s.placeholder(5), s.placeholder(6))

	description := strings.TrimSpace(apiKey.Description)
	_, err := s.db.ExecContext(ctx, query, jti, username, name, description, creationStr, expirationStr)
	if err != nil {
		return fmt.Errorf("failed to insert token metadata: %w", err)
	}
	return nil
}

func (s *SQLStore) InvalidateAll(ctx context.Context, username string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`UPDATE tokens SET expiration_date = %s WHERE username = %s AND expiration_date > %s`,
		s.placeholder(1), s.placeholder(2), s.placeholder(3))

	result, err := s.db.ExecContext(ctx, query, now, username, now)
	if err != nil {
		return fmt.Errorf("failed to mark tokens as expired: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	s.logger.Info("Marked tokens as expired", "count", rows, "user", username)
	return nil
}

func (s *SQLStore) List(ctx context.Context, username string) ([]ApiKeyMetadata, error) {
	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`
	SELECT id, name, COALESCE(description, ''), creation_date, expiration_date
	FROM tokens 
	WHERE username = %s
	ORDER BY creation_date DESC
	`, s.placeholder(1))

	rows, err := s.db.QueryContext(ctx, query, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now()
	tokens := []ApiKeyMetadata{}

	for rows.Next() {
		var t ApiKeyMetadata
		var creationStr, expirationStr string
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &creationStr, &expirationStr); err != nil {
			return nil, err
		}

		t.CreationDate = creationStr
		t.ExpirationDate = expirationStr
		t.Status = computeTokenStatus(expirationStr, now)

		tokens = append(tokens, t)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return tokens, nil
}

func (s *SQLStore) Get(ctx context.Context, jti string) (*ApiKeyMetadata, error) {
	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`
	SELECT id, name, COALESCE(description, ''), creation_date, expiration_date
	FROM tokens 
	WHERE id = %s
	`, s.placeholder(1))

	row := s.db.QueryRowContext(ctx, query, jti)

	var t ApiKeyMetadata
	var creationStr, expirationStr string
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &creationStr, &expirationStr); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}

	t.CreationDate = creationStr
	t.ExpirationDate = expirationStr
	t.Status = computeTokenStatus(expirationStr, time.Now())

	return &t, nil
}

func computeTokenStatus(expirationStr string, now time.Time) string {
	expirationDate, err := time.Parse(time.RFC3339, expirationStr)
	if err != nil || now.After(expirationDate) {
		return TokenStatusExpired
	}
	return TokenStatusActive
}

// AddPermanentKey stores an API key with hash-only storage (no plaintext)
// Per Feature Refinement "Hash-Only Storage": plaintext is NEVER stored, only SHA-256 hash
func (s *SQLStore) AddPermanentKey(ctx context.Context, username, keyID, keyHash, keyPrefix, name, description string) error {
	if keyID == "" {
		return ErrEmptyJTI
	}
	if name == "" {
		return ErrEmptyName
	}
	if keyHash == "" {
		return errors.New("key hash is required")
	}

	creationStr := time.Now().UTC().Format(time.RFC3339)
	// Permanent keys don't expire - use far future date (per arch doc)
	expirationStr := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).Format(time.RFC3339)

	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`
	INSERT INTO tokens (id, username, name, description, creation_date, expiration_date, key_hash, key_prefix, status)
	VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
	`, s.placeholder(1), s.placeholder(2), s.placeholder(3), s.placeholder(4),
		s.placeholder(5), s.placeholder(6), s.placeholder(7), s.placeholder(8), s.placeholder(9))

	_, err := s.db.ExecContext(ctx, query,
		keyID, username, name, description, creationStr, expirationStr, keyHash, keyPrefix, TokenStatusActive)
	if err != nil {
		return fmt.Errorf("failed to insert permanent API key: %w", err)
	}

	s.logger.Debug("Stored permanent API key", "id", keyID, "prefix", keyPrefix, "user", username)
	return nil
}

// GetByHash looks up an API key by its SHA-256 hash (for gateway validation)
// This is the critical path for every authenticated request
// Per Feature Refinement "Instant Revocation": returns error if key is revoked
func (s *SQLStore) GetByHash(ctx context.Context, keyHash string) (*ApiKeyMetadata, error) {
	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`
		SELECT id, username, name, COALESCE(description, ''), COALESCE(key_prefix, ''), COALESCE(status, 'active')
		FROM tokens
		WHERE key_hash = %s
	`, s.placeholder(1))

	row := s.db.QueryRowContext(ctx, query, keyHash)

	var m ApiKeyMetadata
	err := row.Scan(&m.ID, &m.Username, &m.Name, &m.Description, &m.KeyPrefix, &m.Status)

	if err == sql.ErrNoRows {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("database lookup failed: %w", err)
	}

	// Per Feature Refinement "Instant Revocation": reject revoked keys
	if m.Status == TokenStatusRevoked {
		return nil, ErrInvalidKey
	}

	return &m, nil
}

// Revoke marks a specific API key as revoked (soft delete)
// Per Feature Refinement "Individual Key Revocation": keys are soft-deleted for audit trail
func (s *SQLStore) Revoke(ctx context.Context, keyID string) error {
	//nolint:gosec // G201: Safe - using placeholder indices, not user input
	query := fmt.Sprintf(`UPDATE tokens SET status = %s WHERE id = %s`,
		s.placeholder(1), s.placeholder(2))

	result, err := s.db.ExecContext(ctx, query, TokenStatusRevoked, keyID)
	if err != nil {
		return fmt.Errorf("failed to revoke API key: %w", err)
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
