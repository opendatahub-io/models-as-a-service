package api_keys

import (
	"context"
	"errors"
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
	Add(ctx context.Context, username string, apiKey *APIKey) error

	// AddPermanentKey stores a permanent API key with hash-only storage (no plaintext)
	AddPermanentKey(ctx context.Context, username string, keyID, keyHash, keyPrefix, name, description string) error

	List(ctx context.Context, username string) ([]ApiKeyMetadata, error)

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
