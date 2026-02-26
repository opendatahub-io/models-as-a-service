package api_keys

import "github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"

// APIKey represents a full API key with token and metadata.
// It embeds token.Token and adds API key-specific fields.
// Used for legacy K8s ServiceAccount tokens.
type APIKey struct {
	token.Token

	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PermanentAPIKey represents a user-controlled API key (sk-oai-* format).
// Per Feature Refinement "Hash-Only Storage": stores hash-only (plaintext never stored).
type PermanentAPIKey struct {
	ID          string `json:"id"`
	KeyPrefix   string `json:"keyPrefix"` // Display prefix: sk-oai-abc123...
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Username    string `json:"username"`
	CreatedAt   string `json:"createdAt"`
	Status      string `json:"status"` // "active", "revoked"
}

// ApiKeyMetadata represents metadata for a single API key (without the token itself).
// Used for listing and retrieving API key metadata from the database.
// Note: KeyPrefix is NOT included - it's only shown once at creation (show-once pattern).
// Users should identify keys by name/description for security.
type ApiKeyMetadata struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description,omitempty"`
	Username           string   `json:"username,omitempty"`
	TierName           string   `json:"tierName,omitempty"`           // Tier determined at creation time
	OriginalUserGroups []string `json:"originalUserGroups,omitempty"` // User's groups at creation (audit only)
	CreationDate       string   `json:"creationDate"`
	ExpirationDate     string   `json:"expirationDate,omitempty"` // Empty for permanent keys
	Status             string   `json:"status"`                   // "active", "expired", "revoked"
	LastUsedAt         string   `json:"lastUsedAt,omitempty"`     // Tracks when key was last used for validation
}

// ValidationResult holds the result of API key validation (for Authorino HTTP callback).
type ValidationResult struct {
	Valid    bool     `json:"valid"`
	UserID   string   `json:"userId,omitempty"`
	Username string   `json:"username,omitempty"`
	KeyID    string   `json:"keyId,omitempty"`
	Groups   []string `json:"groups,omitempty"` // Synthetic groups for tier-based authorization
	Reason   string   `json:"reason,omitempty"` // If invalid: "key not found", "revoked", etc.
}

// PaginationParams holds query parameters for paginated list requests.
type PaginationParams struct {
	Limit  int
	Offset int
}

// PaginatedResult holds the result of a paginated query.
type PaginatedResult struct {
	Keys    []ApiKeyMetadata
	HasMore bool
}

// ListAPIKeysResponse is the HTTP response for GET /v1/api-keys.
type ListAPIKeysResponse struct {
	Object  string           `json:"object"`   // Always "list"
	Data    []ApiKeyMetadata `json:"data"`
	HasMore bool             `json:"has_more"`
}
