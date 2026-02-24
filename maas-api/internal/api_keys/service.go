package api_keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type Service struct {
	tokenManager *token.Manager
	store        MetadataStore
	logger       *logger.Logger
	config       *config.Config
}

func NewService(tokenManager *token.Manager, store MetadataStore, cfg *config.Config) *Service {
	return &Service{
		tokenManager: tokenManager,
		store:        store,
		logger:       logger.Production(),
		config:       cfg,
	}
}

// NewServiceWithLogger creates a new service with a custom logger (for testing).
func NewServiceWithLogger(tokenManager *token.Manager, store MetadataStore, cfg *config.Config, log *logger.Logger) *Service {
	if log == nil {
		log = logger.Production()
	}
	return &Service{
		tokenManager: tokenManager,
		store:        store,
		logger:       log,
		config:       cfg,
	}
}

// CreateServiceAccountAPIKey creates a K8s ServiceAccount-based API key (legacy, will be removed in future).
// Note: ServiceAccount-based tokens are NOT stored in the database - they are validated via Kubernetes TokenReview API.
func (s *Service) CreateServiceAccountAPIKey(ctx context.Context, user *token.UserContext, name string, description string, expiration time.Duration) (*APIKey, error) {
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

	// Legacy SA tokens are NOT stored in database
	// Authorization is handled by Kubernetes TokenReview API, not database lookup

	return apiKey, nil
}

// CreateAPIKeyResponse is returned when creating an API key.
// Per Feature Refinement "Keys Shown Only Once": plaintext key is ONLY returned at creation time.
type CreateAPIKeyResponse struct {
	Key       string  `json:"key"`       // Plaintext key - SHOWN ONCE, NEVER STORED
	KeyPrefix string  `json:"keyPrefix"` // Display prefix for UI
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt *string `json:"expiresAt,omitempty"` // RFC3339 timestamp
}

// CreateAPIKey creates a new API key (sk-oai-* format).
// Keys can be permanent (expiresIn=nil) or expiring (expiresIn set).
// Per Feature Refinement "Key Format & Security":
// - Generates cryptographically secure key with sk-oai-* prefix
// - Stores ONLY the SHA-256 hash (plaintext never stored)
// - Returns plaintext ONCE at creation ("show-once" pattern)
// - Determines tier from user groups and stores for authorization.
func (s *Service) CreateAPIKey(ctx context.Context, user *token.UserContext, name, description string, expiresIn *time.Duration) (*CreateAPIKeyResponse, error) {
	// Validate expiration based on policy
	if s.config != nil && s.config.APIKeyExpirationPolicy == "required" && expiresIn == nil {
		return nil, errors.New("expiration is required by system policy")
	}
	if expiresIn != nil && *expiresIn <= 0 {
		return nil, errors.New("expiration must be positive")
	}

	// Calculate absolute expiration timestamp
	var expiresAt *time.Time
	if expiresIn != nil {
		expiry := time.Now().UTC().Add(*expiresIn)
		expiresAt = &expiry
	}

	// Generate the API key
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	// Generate unique ID for this key
	keyID := uuid.New().String()

	// Marshal user groups for storage (used for authorization)
	userGroups, err := json.Marshal(user.Groups)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal user groups: %w", err)
	}

	// Store in database (hash only, plaintext NEVER stored)
	if err := s.store.AddKey(ctx, user.Username, keyID, hash, prefix, name, description, string(userGroups), expiresAt); err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	s.logger.Info("Created API key", "user", user.Username, "groups", user.Groups, "id", keyID)

	// Return plaintext to user - THIS IS THE ONLY TIME IT'S AVAILABLE
	response := &CreateAPIKeyResponse{
		Key:       plaintext, // SHOWN ONCE, NEVER AGAIN
		KeyPrefix: prefix,
		ID:        keyID,
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if expiresAt != nil {
		formatted := expiresAt.Format(time.RFC3339)
		response.ExpiresAt = &formatted
	}

	return response, nil
}

// List returns a paginated list of API keys for a user.
// Pagination is mandatory - no unbounded queries allowed.
func (s *Service) List(ctx context.Context, user *token.UserContext, params PaginationParams) (*PaginatedResult, error) {
	return s.store.List(ctx, user.Username, params)
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
// - Returns user identity if valid, rejection reason if invalid.
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
				Reason: "key revoked or expired",
			}, nil
		}
		return nil, fmt.Errorf("validation lookup failed: %w", err)
	}

	// Update last_used_at asynchronously (don't block validation response)
	//nolint:contextcheck // Intentionally using background context - original may be cancelled.
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

	// Return the user's original groups (stored at key creation time)
	// These groups are used directly by subscription-based authorization
	// Note: Groups are immutable - they reflect user's group membership at creation time
	groups := metadata.OriginalUserGroups
	if groups == nil {
		groups = []string{} // Return empty array if no groups stored
	}

	// Success - return user identity and groups for Authorino
	return &ValidationResult{
		Valid:    true,
		UserID:   metadata.Username,
		Username: metadata.Username,
		KeyID:    metadata.ID,
		Groups:   groups, // Original user groups for subscription-based authorization
	}, nil
}

// RevokeAPIKey revokes a specific permanent API key.
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	return s.store.Revoke(ctx, keyID)
}
