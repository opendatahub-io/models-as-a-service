package api_keys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type Service struct {
	tokenManager *token.Manager
	store        MetadataStore
}

func NewService(tokenManager *token.Manager, store MetadataStore) *Service {
	return &Service{
		tokenManager: tokenManager,
		store:        store,
	}
}

// CreateAPIKey creates a K8s ServiceAccount-based API key (legacy, with expiration)
func (s *Service) CreateAPIKey(ctx context.Context, user *token.UserContext, name string, description string, expiration time.Duration) (*APIKey, error) {
	// Generate token
	tok, err := s.tokenManager.GenerateToken(ctx, user, expiration)
	if err != nil {
		return nil, fmt.Errorf("failed to generate token: %w", err)
	}

	// Create APIKey with embedded Token and metadata
	apiKey := &APIKey{
		Token:       *tok,
		Name:        name,
		Description: description,
	}

	if err := s.store.Add(ctx, user.Username, apiKey); err != nil {
		return nil, fmt.Errorf("failed to persist api key metadata: %w", err)
	}

	return apiKey, nil
}

// CreatePermanentKeyResponse is returned when creating an API key
// Per Feature Refinement "Keys Shown Only Once": plaintext key is ONLY returned at creation time
type CreatePermanentKeyResponse struct {
	Key       string `json:"key"`       // Plaintext key - SHOWN ONCE, NEVER STORED
	KeyPrefix string `json:"keyPrefix"` // Display prefix for UI
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

// CreatePermanentAPIKey creates a new API key (sk-oai-* format)
// Per Feature Refinement "Key Format & Security":
// - Generates cryptographically secure key with sk-oai-* prefix
// - Stores ONLY the SHA-256 hash (plaintext never stored)
// - Returns plaintext ONCE at creation ("show-once" pattern)
func (s *Service) CreatePermanentAPIKey(ctx context.Context, user *token.UserContext, name, description string) (*CreatePermanentKeyResponse, error) {
	// Generate the API key
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	// Generate unique ID for this key
	keyID := uuid.New().String()

	// Store in database (hash only, plaintext NEVER stored)
	if err := s.store.AddPermanentKey(ctx, user.Username, keyID, hash, prefix, name, description); err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	// Return plaintext to user - THIS IS THE ONLY TIME IT'S AVAILABLE
	return &CreatePermanentKeyResponse{
		Key:       plaintext, // SHOWN ONCE, NEVER AGAIN
		KeyPrefix: prefix,
		ID:        keyID,
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func (s *Service) ListAPIKeys(ctx context.Context, user *token.UserContext) ([]ApiKeyMetadata, error) {
	return s.store.List(ctx, user.Username)
}

func (s *Service) GetAPIKey(ctx context.Context, id string) (*ApiKeyMetadata, error) {
	return s.store.Get(ctx, id)
}

// RevokeAll invalidates all tokens for the user (ephemeral and persistent).
// It recreates the Service Account (invalidating all tokens) and marks API key metadata as expired.
func (s *Service) RevokeAll(ctx context.Context, user *token.UserContext) error {
	// Revoke in K8s (recreate SA) - this invalidates all tokens
	if err := s.tokenManager.RevokeTokens(ctx, user); err != nil {
		return fmt.Errorf("failed to revoke tokens in k8s: %w", err)
	}

	// Mark API key metadata as expired (preserves history)
	if err := s.store.InvalidateAll(ctx, user.Username); err != nil {
		return fmt.Errorf("tokens revoked but failed to mark metadata as expired: %w", err)
	}

	return nil
}

// ValidateAPIKey validates an API key by hash lookup (called by Authorino HTTP callback)
// Per Feature Refinement "Gateway Integration (Inference Flow)":
// - Computes SHA-256 hash of incoming key
// - Looks up hash in database
// - Returns user identity if valid, rejection reason if invalid
func (s *Service) ValidateAPIKey(ctx context.Context, key string) (*ValidationResult, error) {
	// Check key format
	if !IsValidKeyFormat(key) {
		return &ValidationResult{
			Valid:  false,
			Reason: "invalid key format",
		}, nil
	}

	// Compute hash of incoming key
	hash := HashAPIKey(key)

	// Lookup in database
	metadata, err := s.store.GetByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return &ValidationResult{
				Valid:  false,
				Reason: "key not found",
			}, nil
		}
		if errors.Is(err, ErrInvalidKey) {
			return &ValidationResult{
				Valid:  false,
				Reason: "key revoked",
			}, nil
		}
		return nil, fmt.Errorf("validation lookup failed: %w", err)
	}

	// Success - return user identity for Authorino
	return &ValidationResult{
		Valid:    true,
		UserID:   metadata.Username,
		Username: metadata.Username,
		KeyID:    metadata.ID,
	}, nil
}

// RevokeAPIKey revokes a specific permanent API key
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	return s.store.Revoke(ctx, keyID)
}
