package api_keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tier"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type Service struct {
	tokenManager *token.Manager
	store        MetadataStore
	tierMapper   *tier.Mapper
	logger       *logger.Logger
}

func NewService(tokenManager *token.Manager, store MetadataStore, tierMapper *tier.Mapper) *Service {
	return &Service{
		tokenManager: tokenManager,
		store:        store,
		tierMapper:   tierMapper,
		logger:       logger.Production(),
	}
}

// NewServiceWithLogger creates a new service with a custom logger (for testing)
func NewServiceWithLogger(tokenManager *token.Manager, store MetadataStore, tierMapper *tier.Mapper, log *logger.Logger) *Service {
	if log == nil {
		log = logger.Production()
	}
	return &Service{
		tokenManager: tokenManager,
		store:        store,
		tierMapper:   tierMapper,
		logger:       log,
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
// - Determines tier from user groups and stores for authorization
func (s *Service) CreatePermanentAPIKey(ctx context.Context, user *token.UserContext, name, description string) (*CreatePermanentKeyResponse, error) {
	// Generate the API key
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	// Determine tier from user groups (same logic as token generation)
	userTier, err := s.tierMapper.GetTierForGroups(user.Groups...)
	if err != nil {
		return nil, fmt.Errorf("failed to determine user tier: %w", err)
	}

	// Generate unique ID for this key
	keyID := uuid.New().String()

	// Marshal original user groups for audit trail (optional)
	originalGroups, _ := json.Marshal(user.Groups)

	// Store in database (hash only, plaintext NEVER stored)
	if err := s.store.AddPermanentKey(ctx, user.Username, keyID, hash, prefix, name, description, userTier.Name, string(originalGroups)); err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	s.logger.Info("Created permanent API key", "user", user.Username, "tier", userTier.Name, "id", keyID)

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

	// Update last_used_at asynchronously (don't block validation response)
	go func() {
		// Recover from panics to prevent crashing the entire process
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("Panic in UpdateLastUsed goroutine", "panic", r, "key_id", metadata.ID)
			}
		}()

		// Use background context with timeout since original may be cancelled
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := s.store.UpdateLastUsed(ctx, metadata.ID); err != nil {
			// Log warning but don't fail validation - this is best-effort tracking
			s.logger.Warn("Failed to update last_used_at", "key_id", metadata.ID, "error", err)
		}
	}()

	// Generate synthetic ServiceAccount group from tier name
	// This matches the pattern used by actual ServiceAccount tokens
	var groups []string
	if metadata.TierName != "" {
		syntheticGroup := fmt.Sprintf("system:serviceaccounts:maas-default-gateway-tier-%s", metadata.TierName)
		groups = []string{syntheticGroup}
	}

	// Success - return user identity and groups for Authorino
	return &ValidationResult{
		Valid:    true,
		UserID:   metadata.Username,
		Username: metadata.Username,
		KeyID:    metadata.ID,
		Groups:   groups, // Synthetic groups for tier lookup and authorization
	}, nil
}

// RevokeAPIKey revokes a specific permanent API key
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	return s.store.Revoke(ctx, keyID)
}
