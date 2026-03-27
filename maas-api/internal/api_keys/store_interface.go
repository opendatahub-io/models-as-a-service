package api_keys

import (
	"context"
	"errors"
	"time"
)

var (
	ErrTokenNotFound = errors.New("token not found")
	ErrKeyNotFound   = errors.New("api key not found")
	ErrInvalidKey    = errors.New("api key is invalid or revoked")
	ErrEmptyJTI      = errors.New("key ID is required and cannot be empty")
	ErrEmptyName     = errors.New("key name is required and cannot be empty")

	// Expiration validation errors.
	ErrExpirationNotPositive = errors.New("expiration must be positive")
	ErrExpirationExceedsMax  = errors.New("expiration exceeds maximum allowed")
)

// Legacy constants for backward compatibility with database operations.
// Prefer using Status enum constants in new code.
const (
	TokenStatusActive  = "active"
	TokenStatusExpired = "expired"
	TokenStatusRevoked = "revoked"
)

type MetadataStore interface {
	// AddKey stores an API key with hash-only storage (no plaintext).
	// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
	//
	// Parameters:
	//   - keyID: Database UUID/JTI (primary key), distinct from the embedded salt in the API key
	//   - keyHash: SHA-256(embedded_key_id + "\x00" + secret), where embedded_key_id is the
	//     per-key salt encoded in the API key format (sk-oai-{embedded_key_id}_{secret})
	//   - userGroups: array of user's groups (used for authorization)
	//   - ephemeral: marks the key as short-lived for programmatic use
	//
	// Note: keyPrefix is NOT stored (security - reduces brute-force attack surface).
	AddKey(ctx context.Context, username string, keyID, keyHash, name, description string, userGroups []string, subscription string, expiresAt *time.Time, ephemeral bool) error

	// Search returns API keys matching the search criteria.
	// Supports filtering, sorting, and pagination.
	Search(
		ctx context.Context,
		username string,
		filters *SearchFilters,
		sort *SortParams,
		pagination *PaginationParams,
	) (*PaginatedResult, error)

	Get(ctx context.Context, jti string) (*ApiKey, error)

	// GetByHash looks up an API key by its SHA-256 hash (for Authorino validation).
	// Hash is computed as SHA-256(embedded_key_id + "\x00" + secret) where embedded_key_id
	// is the per-key salt encoded in the API key format (sk-oai-{embedded_key_id}_{secret}).
	// Returns ErrKeyNotFound if key doesn't exist, ErrInvalidKey if revoked or expired.
	GetByHash(ctx context.Context, keyHash string) (*ApiKey, error)

	// InvalidateAll marks all active tokens for a user as revoked.
	// Returns the count of keys that were revoked.
	InvalidateAll(ctx context.Context, username string) (int, error)

	// Revoke marks a specific API key as revoked (status transition: active → revoked).
	Revoke(ctx context.Context, keyID string) error

	// UpdateLastUsed updates the last_used_at timestamp for an API key.
	// Called after successful validation to track key usage.
	UpdateLastUsed(ctx context.Context, keyID string) error

	Close() error
}
