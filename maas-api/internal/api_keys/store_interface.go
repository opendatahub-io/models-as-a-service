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
	AddKey(ctx context.Context,
		username string,
		keyID,
		keyHash,
		name,
		description string,
		userGroups []string,
		subscription,
		tenant string,
		expiresAt *time.Time,
		ephemeral bool) error

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

	// GetForTenant retrieves one API key by ID scoped to a tenant.
	GetForTenant(ctx context.Context, tenant string, keyID string) (*ApiKey, error)

	// GetByHash looks up an API key by its SHA-256 hash (for Authorino validation).
	// Hash is computed as SHA-256(embedded_key_id + "\x00" + secret) where embedded_key_id
	// is the per-key salt encoded in the API key format (sk-oai-{embedded_key_id}_{secret}).
	// Returns ErrKeyNotFound if key doesn't exist, ErrInvalidKey if revoked or expired.
	GetByHash(ctx context.Context, keyHash string) (*ApiKey, error)

	// GetByHashForTenant looks up an API key by hash scoped to a tenant.
	GetByHashForTenant(ctx context.Context, keyHash string, tenant string) (*ApiKey, error)

	// SearchForTenant returns API keys matching the search criteria scoped to a tenant.
	SearchForTenant(
		ctx context.Context,
		tenant string,
		username string,
		filters *SearchFilters,
		sort *SortParams,
		pagination *PaginationParams,
	) (*PaginatedResult, error)

	// InvalidateAll marks all active tokens for a user as revoked.
	// Returns the count of keys that were revoked.
	InvalidateAll(ctx context.Context, username string) (int, error)

	// InvalidateAllForTenant marks all active tokens for a user in a tenant as revoked.
	InvalidateAllForTenant(ctx context.Context, tenant string, username string) (int, error)

	// Revoke marks a specific API key as revoked (status transition: active → revoked).
	Revoke(ctx context.Context, keyID string) error

	// RevokeForTenant marks a specific API key as revoked within a tenant.
	RevokeForTenant(ctx context.Context, tenant string, keyID string) error

	// UpdateLastUsed updates the last_used_at timestamp for an API key.
	// Called after successful validation to track key usage.
	UpdateLastUsed(ctx context.Context, keyID string) error

	// UpdateLastUsedForTenant updates last_used_at for an API key within a tenant.
	UpdateLastUsedForTenant(ctx context.Context, tenant string, keyID string) error

	// DeleteExpiredEphemeral removes expired ephemeral API keys from storage.
	// Deletes keys where ephemeral=TRUE AND (status='expired' OR expires_at < NOW()).
	// Returns the count of deleted keys.
	DeleteExpiredEphemeral(ctx context.Context) (int64, error)

	// DeleteExpiredEphemeralForTenant removes expired ephemeral API keys in a tenant.
	DeleteExpiredEphemeralForTenant(ctx context.Context, tenant string) (int64, error)

	Close() error
}
