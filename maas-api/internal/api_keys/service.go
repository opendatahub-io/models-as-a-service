package api_keys

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// SubscriptionSelector resolves which MaaSSubscription to bind when minting an API key.
type SubscriptionSelector interface {
	Select(groups []string, username string, requestedSubscription string, requestedModel string) (*subscription.SelectResponse, error)
	SelectHighestPriority(groups []string, username string) (*subscription.SelectResponse, error)
}

type Service struct {
	store       MetadataStore
	logger      *logger.Logger
	config      *config.Config
	subSelector SubscriptionSelector
}

func NewService(store MetadataStore, cfg *config.Config, sub SubscriptionSelector) *Service {
	return NewServiceWithLogger(store, cfg, sub, logger.Production())
}

// NewServiceWithLogger creates a new service with a custom logger (for testing).
func NewServiceWithLogger(store MetadataStore, cfg *config.Config, sub SubscriptionSelector, log *logger.Logger) *Service {
	if log == nil {
		log = logger.Production()
	}
	return &Service{
		store:       store,
		logger:      log,
		config:      cfg,
		subSelector: sub,
	}
}

// CreateAPIKeyResponse is returned when creating an API key.
// Per Feature Refinement "Keys Shown Only Once": plaintext key is ONLY returned at creation time.
type CreateAPIKeyResponse struct {
	Key          string  `json:"key"`       // Plaintext key - SHOWN ONCE, NEVER STORED
	KeyPrefix    string  `json:"keyPrefix"` // Display prefix for UI
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Subscription string  `json:"subscription"` // MaaSSubscription name bound to this key
	CreatedAt    string  `json:"createdAt"`
	ExpiresAt    *string `json:"expiresAt,omitempty"` // RFC3339 timestamp
	Ephemeral    bool    `json:"ephemeral"`           // Short-lived programmatic key
}

// CreateAPIKey creates a new API key (sk-oai-* format).
// If expiresIn is not provided, defaults to APIKeyMaxExpirationDays (or 1hr for ephemeral).
// Per Feature Refinement "Key Format & Security":
// - Generates cryptographically secure key with sk-oai-* prefix
// - Stores ONLY the SHA-256 hash (plaintext never stored)
// - Returns plaintext ONCE at creation ("show-once" pattern)
// - Stores user groups for subscription-based authorization.
// Admins can create keys for other users by specifying a different username.
func (s *Service) CreateAPIKey(
	ctx context.Context, username string, userGroups []string, name, description string,
	expiresIn *time.Duration, ephemeral bool, requestedSubscription string,
) (*CreateAPIKeyResponse, error) {
	// Compute max expiration days once from config-or-default (CWE-613 mitigation).
	maxDays := constant.DefaultAPIKeyMaxExpirationDays
	if s.config != nil && s.config.APIKeyMaxExpirationDays > 0 {
		maxDays = s.config.APIKeyMaxExpirationDays
	}
	maxRegularDuration := time.Duration(maxDays) * 24 * time.Hour

	// Default expiration if not provided
	if expiresIn == nil {
		if ephemeral {
			// Ephemeral keys default to 1 hour
			d := 1 * time.Hour
			expiresIn = &d
		} else {
			// Regular keys default to max expiration days
			expiresIn = &maxRegularDuration
		}
	}

	if *expiresIn <= 0 {
		return nil, ErrExpirationNotPositive
	}

	// Validate against maximum expiration limit (always enforced)
	if ephemeral {
		// Ephemeral keys have a strict 1-hour maximum to prevent abuse
		maxEphemeralDuration := 1 * time.Hour
		if *expiresIn > maxEphemeralDuration {
			return nil, fmt.Errorf("ephemeral key expiration (%v) cannot exceed 1 hour: %w", *expiresIn, ErrExpirationExceedsMax)
		}
	} else if *expiresIn > maxRegularDuration {
		// Regular keys always enforce max expiration (config or default)
		return nil, fmt.Errorf("requested expiration (%v) exceeds maximum allowed (%d days): %w",
			*expiresIn, maxDays, ErrExpirationExceedsMax)
	}

	// Calculate absolute expiration timestamp (always set since we default to max)
	expiresAt := time.Now().UTC().Add(*expiresIn)

	// Generate the API key with embedded key_id (used as per-key salt).
	// Format: sk-oai-{key_id}_{secret}
	// Hash: SHA-256(key_id + "\x00" + secret) - null delimiter prevents length-ambiguity
	// Note: key_id here is the embedded salt in the API key, distinct from keyID (DB UUID) below.
	plaintext, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	var subResp *subscription.SelectResponse
	var selectErr error
	if requestedSubscription != "" {
		//nolint:unqueryvet,nolintlint // Select is subscription resolution, not a SQL query
		subResp, selectErr = s.subSelector.Select(userGroups, username, requestedSubscription, "")
	} else {
		subResp, selectErr = s.subSelector.SelectHighestPriority(userGroups, username)
	}
	if selectErr != nil {
		s.logger.Warn("Subscription selection failed when creating API key",
			"user", username,
			"requestedSubscription", requestedSubscription,
			"error", selectErr,
		)
		return nil, selectErr
	}
	subscriptionName := subResp.Name

	// Generate unique ID for this key
	keyID := uuid.New().String()

	// Store in database (hash only, plaintext NEVER stored)
	// Note: prefix is NOT stored (security - reduces brute-force attack surface)
	// userGroups stored as PostgreSQL TEXT[] array (no JSON marshaling needed)
	// Hash is SHA-256(key_id + secret) where key_id is embedded in the API key as per-key salt
	if err := s.store.AddKey(ctx, username, keyID, hash, name, description, userGroups, subscriptionName, &expiresAt, ephemeral); err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	s.logger.Info("Created API key", "user", username, "groups", userGroups, "id", keyID, "ephemeral", ephemeral)

	// Return plaintext to user - THIS IS THE ONLY TIME IT'S AVAILABLE
	formatted := expiresAt.Format(time.RFC3339)
	response := &CreateAPIKeyResponse{
		Key:          plaintext, // SHOWN ONCE, NEVER AGAIN
		KeyPrefix:    prefix,
		ID:           keyID,
		Name:         name,
		Subscription: subscriptionName,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:    &formatted,
		Ephemeral:    ephemeral,
	}

	return response, nil
}

func (s *Service) GetAPIKey(ctx context.Context, id string) (*ApiKey, error) {
	return s.store.Get(ctx, id)
}

// ValidateAPIKey validates an API key (called by Authorino HTTP callback).
// Per Feature Refinement "Gateway Integration (Inference Flow)":
// - Parses the key to extract key_id and secret
// - Computes SHA-256(key_id + secret) - key_id acts as per-key salt
// - Looks up by hash (O(1) indexed lookup)
// - Returns user identity if valid, rejection reason if invalid.
func (s *Service) ValidateAPIKey(ctx context.Context, key string) (*ValidationResult, error) {
	// Check key format
	if !IsValidKeyFormat(key) {
		return &ValidationResult{
			Valid:  false,
			Reason: "invalid key format",
		}, nil
	}

	// Compute salted hash: SHA-256(key_id + secret)
	// key_id is embedded in the API key and serves as per-key salt
	hash := HashAPIKey(key)
	if hash == "" {
		return &ValidationResult{
			Valid:  false,
			Reason: "invalid key format",
		}, nil
	}

	// Lookup by hash (O(1) indexed lookup)
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

	// Return the user's groups (stored at key creation time)
	// These groups are used directly by subscription-based authorization
	// Note: Groups are immutable - they reflect user's group membership at creation time
	groups := metadata.Groups
	if groups == nil {
		groups = []string{} // Return empty array if no groups stored
	}

	// Fail closed: reject keys with no bound subscription (CWE-284)
	// This prevents legacy keys, bad migrations, or manual writes with empty subscription
	// from bypassing the "subscription bound at mint" access control invariant
	if strings.TrimSpace(metadata.Subscription) == "" {
		s.logger.Warn("API key missing bound subscription", "key_id", metadata.ID)
		return &ValidationResult{
			Valid:  false,
			Reason: "key has no subscription bound",
		}, nil
	}

	// Success - return user identity and groups for Authorino
	return &ValidationResult{
		Valid:        true,
		UserID:       metadata.Username,
		Username:     metadata.Username,
		KeyID:        metadata.ID,
		Groups:       groups, // Original user groups for subscription-based authorization
		Subscription: metadata.Subscription,
	}, nil
}

// RevokeAPIKey revokes a specific permanent API key.
func (s *Service) RevokeAPIKey(ctx context.Context, keyID string) error {
	return s.store.Revoke(ctx, keyID)
}

// Search searches API keys with flexible filtering, sorting, and pagination.
func (s *Service) Search(
	ctx context.Context,
	username string,
	filters *SearchFilters,
	sort *SortParams,
	pagination *PaginationParams,
) (*PaginatedResult, error) {
	return s.store.Search(ctx, username, filters, sort, pagination)
}

// BulkRevokeAPIKeys revokes all active keys for a user
// Returns count of revoked keys.
func (s *Service) BulkRevokeAPIKeys(ctx context.Context, username string) (int, error) {
	if username == "" {
		return 0, errors.New("username is required")
	}
	return s.store.InvalidateAll(ctx, username)
}
