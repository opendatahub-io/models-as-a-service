package api_keys

// Status represents the lifecycle state of an API key.
// API keys follow a one-way state transition: active → revoked/expired.
type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
	StatusExpired Status = "expired"
)

// String returns the string representation of the status.
func (s Status) String() string {
	return string(s)
}

// ApiKey represents metadata for a single API key (without the token itself).
// Used for listing and retrieving API key metadata from the database.
// Note: KeyPrefix is NOT included - it's only shown once at creation (show-once pattern).
// Users should identify keys by name/description for security.
type ApiKey struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Username       string   `json:"username,omitempty"`
	Subscription   string   `json:"subscription,omitempty"`   // MaaSSubscription name bound at mint time
	Groups         []string `json:"groups,omitempty"`         // User's groups at creation (immutable snapshot for authorization)
	CreationDate   string   `json:"creationDate"`
	ExpirationDate string   `json:"expirationDate,omitempty"` // Empty for permanent keys
	Status         Status   `json:"status"`                   // "active", "expired", "revoked"
	LastUsedAt     string   `json:"lastUsedAt,omitempty"`     // Tracks when key was last used for validation
	Ephemeral      bool     `json:"ephemeral"`                // Short-lived programmatic key
}

// ValidationResult holds the result of API key validation (for Authorino HTTP callback).
type ValidationResult struct {
	Valid        bool     `json:"valid"`
	UserID       string   `json:"userId,omitempty"`
	Username     string   `json:"username,omitempty"`
	KeyID        string   `json:"keyId,omitempty"`
	Groups       []string `json:"groups,omitempty"`       // User groups for subscription-based authorization
	Subscription string   `json:"subscription,omitempty"` // MaaSSubscription name from DB (Authorino → subscription-info)
	Reason       string   `json:"reason,omitempty"`       // If invalid: "key not found", "revoked", etc.
}

// PaginationParams holds pagination parameters.
type PaginationParams struct {
	Limit  int `json:"limit"`  // Default 50, max 100
	Offset int `json:"offset"` // Default 0
}

// PaginatedResult holds the result of a paginated query.
type PaginatedResult struct {
	Keys    []ApiKey
	HasMore bool
}

// ============================================================
// SEARCH REQUEST/RESPONSE TYPES
// ============================================================

// SearchAPIKeysRequest for POST /v1/api-keys/search.
type SearchAPIKeysRequest struct {
	Filters    *SearchFilters    `json:"filters,omitempty"`
	Sort       *SortParams       `json:"sort,omitempty"`
	Pagination *PaginationParams `json:"pagination,omitempty"`
}

// SearchFilters holds all filter criteria for API key search.
type SearchFilters struct {
	// Phase 1: Core filters
	Username string   `json:"username,omitempty"` // Admin-only filter
	Status   []string `json:"status,omitempty"`   // active, revoked, expired

	// Phase 2: Date range filters (future)
	CreatedAfter  *string `json:"createdAfter,omitempty"`  // RFC3339
	CreatedBefore *string `json:"createdBefore,omitempty"` // RFC3339
	ExpiresAfter  *string `json:"expiresAfter,omitempty"`  // RFC3339
	ExpiresBefore *string `json:"expiresBefore,omitempty"` // RFC3339
	LastUsedAfter *string `json:"lastUsedAfter,omitempty"` // RFC3339

	// Phase 3: Text search (future)
	NameContains        *string `json:"nameContains,omitempty"`
	DescriptionContains *string `json:"descriptionContains,omitempty"`

	// Phase 4: Boolean filters (future)
	HasExpiration *bool `json:"hasExpiration,omitempty"` // true = expiring, false = permanent
	HasBeenUsed   *bool `json:"hasBeenUsed,omitempty"`   // true = used, false = never used

	// Ephemeral key filter
	IncludeEphemeral *bool `json:"includeEphemeral,omitempty"` // Include ephemeral keys in results (default: false)
}

// SortParams specifies sorting criteria.
type SortParams struct {
	By    string `json:"by"`    // created_at, expires_at, last_used_at, name
	Order string `json:"order"` // asc, desc
}

// Default values.
const (
	DefaultSortBy    = "created_at"
	DefaultSortOrder = "desc"
	SortOrderAsc     = "asc"
	SortOrderDesc    = "desc"
	DefaultLimit     = 50
	MaxLimit         = 100
)

// ValidSortFields prevents SQL injection via allowlist.
var ValidSortFields = map[string]bool{
	"created_at":   true,
	"expires_at":   true,
	"last_used_at": true,
	"name":         true,
}

// ValidSortOrders allowlist for sort direction.
var ValidSortOrders = map[string]bool{
	"asc":  true,
	"desc": true,
}

// ValidStatuses allowlist for status filtering.
var ValidStatuses = map[string]bool{
	"active":  true,
	"revoked": true,
	"expired": true,
}

// SearchAPIKeysResponse is the HTTP response for POST /v1/api-keys/search.
type SearchAPIKeysResponse struct {
	Object  string   `json:"object"` // Always "list"
	Data    []ApiKey `json:"data"`
	HasMore bool     `json:"has_more"`
}

// ============================================================
// BULK REVOKE TYPES
// ============================================================

// BulkRevokeRequest for POST /v1/api-keys/bulk-revoke.
type BulkRevokeRequest struct {
	Username string `binding:"required" json:"username"`
}

// BulkRevokeResponse returns count of revoked keys.
type BulkRevokeResponse struct {
	RevokedCount int    `json:"revokedCount"`
	Message      string `json:"message"`
}
