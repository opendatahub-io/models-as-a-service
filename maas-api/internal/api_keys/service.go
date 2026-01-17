package api_keys

import (
	"context"
	"fmt"
	"time"

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
