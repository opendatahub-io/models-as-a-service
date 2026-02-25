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
)

const (
	TokenStatusActive  = "active"
	TokenStatusExpired = "expired"
	TokenStatusRevoked = "revoked"
)

type MetadataStore interface {
	// Add stores a legacy API key (ServiceAccount-based tokens)
	// userGroups is a JSON array of user's groups (used for authorization)
	Add(ctx context.Context, username string, apiKey *APIKey, userGroups string) error

	// AddKey stores an API key with hash-only storage (no plaintext).
	// Keys can be permanent (expiresAt=nil) or expiring (expiresAt set).
	// userGroups is a JSON array of user's groups (used for authorization).
	// Note: keyPrefix is NOT stored (security - reduces brute-force attack surface).
	AddKey(ctx context.Context, username string, keyID, keyHash, name, description, userGroups string, expiresAt *time.Time) error

	// List returns a paginated list of API keys with optional filtering.
	// Pagination is mandatory - no unbounded queries allowed.
	// username can be empty (admin viewing all users) or specific username.
	// statuses can filter by status (active, revoked, expired) - empty means all statuses.
	List(ctx context.Context, username string, params PaginationParams, statuses []string) (*PaginatedResult, error)

	Get(ctx context.Context, jti string) (*ApiKeyMetadata, error)

	// GetByHash looks up an API key by its SHA-256 hash (for Authorino validation)
	// Returns ErrKeyNotFound if key doesn't exist, ErrInvalidKey if revoked
	GetByHash(ctx context.Context, keyHash string) (*ApiKeyMetadata, error)

	// InvalidateAll marks all active tokens for a user as expired.
	InvalidateAll(ctx context.Context, username string) error

	// Revoke marks a specific API key as revoked (soft delete)
	Revoke(ctx context.Context, keyID string) error

	// UpdateLastUsed updates the last_used_at timestamp for an API key
	// Called after successful validation to track key usage
	UpdateLastUsed(ctx context.Context, keyID string) error

	Close() error
}
