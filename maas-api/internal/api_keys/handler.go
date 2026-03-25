package api_keys

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// AdminChecker is an interface for checking if a user is an admin.
// The SARAdminChecker implementation uses Kubernetes SubjectAccessReview
// to check if the user can create maasauthpolicies (RBAC-based admin detection).
type AdminChecker interface {
	IsAdmin(ctx context.Context, user *token.UserContext) bool
}

type Handler struct {
	service      *Service
	logger       *logger.Logger
	adminChecker AdminChecker
}

func NewHandler(log *logger.Logger, service *Service, adminChecker AdminChecker) *Handler {
	if log == nil {
		log = logger.Production()
	}
	if adminChecker == nil {
		panic("adminChecker cannot be nil")
	}
	return &Handler{
		service:      service,
		logger:       log,
		adminChecker: adminChecker,
	}
}

// getUserContext extracts and validates the user context from the Gin context.
// Returns the user context on success, or responds with an error and returns nil.
func (h *Handler) getUserContext(c *gin.Context) *token.UserContext {
	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return nil
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return nil
	}

	return user
}

// isAdmin checks if the user has admin privileges via SubjectAccessReview.
// Admin is determined by RBAC: can user create maasauthpolicies in the configured MaaS namespace?
// Returns true if the user has admin RBAC permissions, false otherwise.
func (h *Handler) isAdmin(ctx context.Context, user *token.UserContext) bool {
	if h == nil || user == nil {
		return false
	}
	return h.adminChecker.IsAdmin(ctx, user)
}

// isAuthorizedForKey checks if the user is authorized to access the API key.
// User is authorized if they own the key or are an admin.
func (h *Handler) isAuthorizedForKey(ctx context.Context, user *token.UserContext, keyOwner string) bool {
	// Check if user owns the key
	if user.Username == keyOwner {
		return true
	}

	// Check if user is admin
	return h.isAdmin(ctx, user)
}

func (h *Handler) GetAPIKey(c *gin.Context) {
	tokenID := c.Param("id")
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Token ID required"})
		return
	}

	// Extract user context for authorization
	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Get the API key to check ownership
	tok, err := h.service.GetAPIKey(c.Request.Context(), tokenID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to get API key",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
		return
	}

	// Check authorization - user must own the key or be admin
	if !h.isAuthorizedForKey(c.Request.Context(), user, tok.Username) {
		h.logger.Warn("Unauthorized API key access attempt",
			"requestingUser", user.Username,
			"keyOwner", tok.Username,
			"keyId", tokenID,
		)
		// Return 404 instead of 403 to prevent key enumeration (IDOR protection)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	c.JSON(http.StatusOK, tok)
}

// CreateAPIKeyRequest is the request body for creating an API key.
// Name is required for regular keys but optional for ephemeral keys.
// If expiresIn is not provided, defaults to API_KEY_MAX_EXPIRATION_DAYS (or 1hr for ephemeral).
// Users can only create keys for themselves - the key inherits the user's groups.
type CreateAPIKeyRequest struct {
	Name        string          `json:"name,omitempty"`        // Required for regular keys, optional for ephemeral
	Description string          `json:"description,omitempty"`
	ExpiresIn   *token.Duration `json:"expiresIn,omitempty"`   // Optional - defaults to API_KEY_MAX_EXPIRATION_DAYS (1hr for ephemeral)
	Ephemeral   bool            `json:"ephemeral,omitempty"`   // Short-lived programmatic token (default: false)
}

// CreateAPIKey handles POST /v1/api-keys
// Creates a new API key (sk-oai-* format) per Feature Refinement.
// If expiresIn is not provided, defaults to API_KEY_MAX_EXPIRATION_DAYS (1hr for ephemeral).
// Per "Keys Shown Only Once": key is returned ONCE at creation and never again.
// Users can only create keys for themselves - the key inherits the user's groups.
func (h *Handler) CreateAPIKey(c *gin.Context) {
	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Validate name requirement for non-ephemeral keys
	if !req.Ephemeral && req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required for non-ephemeral keys"})
		return
	}

	// Auto-generate name for ephemeral keys if not provided
	name := req.Name
	if req.Ephemeral && name == "" {
		name = fmt.Sprintf("ephemeral-%d", time.Now().UnixNano())
	}

	// Parse expiration duration if provided
	var expiresIn *time.Duration
	if req.ExpiresIn != nil {
		d := req.ExpiresIn.Duration
		expiresIn = &d
	} else if req.Ephemeral {
		// Default 1hr expiration for ephemeral keys
		d := 1 * time.Hour
		expiresIn = &d
	}

	// Create key for the authenticated user with their groups
	result, err := h.service.CreateAPIKey(c.Request.Context(), user.Username, user.Groups, name, req.Description, expiresIn, req.Ephemeral)
	if err != nil {
		h.logger.Error("Failed to create API key", "error", err)
		// Return 400 for validation errors, 500 for internal errors
		if errors.Is(err, ErrExpirationNotPositive) || errors.Is(err, ErrExpirationExceedsMax) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
		return
	}

	h.logger.Info("Created API key",
		"keyId", result.ID,
		"keyPrefix", result.KeyPrefix,
		"username", user.Username,
		"groups", user.Groups,
		"ephemeral", req.Ephemeral,
	)

	// Return the key - THIS IS THE ONLY TIME THE PLAINTEXT IS SHOWN
	c.JSON(http.StatusCreated, result)
}

// ValidateAPIKeyRequest is the request body for validating an API key.
type ValidateAPIKeyRequest struct {
	Key string `binding:"required" json:"key"`
}

// ValidateAPIKeyHandler handles POST /internal/v1/api-keys/validate
// This endpoint is called by Authorino via HTTP external auth callback
// Per Feature Refinement "Gateway Integration (Inference Flow)".
func (h *Handler) ValidateAPIKeyHandler(c *gin.Context) {
	var req ValidateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}

	result, err := h.service.ValidateAPIKey(c.Request.Context(), req.Key)
	if err != nil {
		h.logger.Error("API key validation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "validation failed"})
		return
	}

	if !result.Valid {
		// Return 200 with validation result for Authorino
		// Per design doc section 7.7: invalid keys should return 200 with valid:false
		c.JSON(http.StatusOK, result)
		return
	}

	// Valid key - return user identity for Authorino to use
	c.JSON(http.StatusOK, result)
}

// RevokeAPIKey handles DELETE /v1/api-keys/:id
// Revokes a specific API key by changing its status to 'revoked'.
func (h *Handler) RevokeAPIKey(c *gin.Context) {
	keyID := c.Param("id")
	if keyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API key ID required"})
		return
	}

	// Extract user context for authorization
	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Get the API key to check ownership before revoking
	keyMetadata, err := h.service.GetAPIKey(c.Request.Context(), keyID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to get API key for authorization check", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
		return
	}

	// Check authorization - user must own the key or be admin
	if !h.isAuthorizedForKey(c.Request.Context(), user, keyMetadata.Username) {
		h.logger.Warn("Unauthorized API key revocation attempt",
			"requestingUser", user.Username,
			"keyOwner", keyMetadata.Username,
			"keyId", keyID,
		)
		// Return 404 instead of 403 to prevent key enumeration (IDOR protection)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	// Perform the revocation
	if err := h.service.RevokeAPIKey(c.Request.Context(), keyID); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to revoke API key", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke API key"})
		return
	}

	h.logger.Info("Revoked API key", "keyId", keyID, "revokedBy", user.Username)

	// Return the revoked key metadata (per OpenAPI spec)
	revokedKey, err := h.service.GetAPIKey(c.Request.Context(), keyID)
	if err != nil {
		h.logger.Error("Failed to retrieve revoked key", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Key revoked but failed to retrieve metadata"})
		return
	}

	c.JSON(http.StatusOK, revokedKey)
}

// SearchAPIKeys handles POST /v1/api-keys/search
// Searches API keys with flexible filtering, sorting, and pagination.
func (h *Handler) SearchAPIKeys(c *gin.Context) {
	var req SearchAPIKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Apply defaults if not provided
	if req.Filters == nil {
		req.Filters = &SearchFilters{}
	}
	if req.Sort == nil {
		req.Sort = &SortParams{
			By:    DefaultSortBy,
			Order: DefaultSortOrder,
		}
	}
	if req.Pagination == nil {
		req.Pagination = &PaginationParams{
			Limit:  DefaultLimit,
			Offset: 0,
		}
	}

	// Validate status values (if status not specified, all statuses are returned)
	for _, status := range req.Filters.Status {
		trimmed := strings.TrimSpace(status)
		if !ValidStatuses[trimmed] {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid status '%s': must be active, revoked, or expired", status),
			})
			return
		}
	}

	// Determine target username for filtering
	isAdmin := h.isAdmin(c.Request.Context(), user)
	targetUsername := req.Filters.Username

	if !isAdmin {
		// Regular user: can only search own keys
		if targetUsername != "" && targetUsername != user.Username {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "non-admin users can only search their own API keys",
			})
			return
		}
		// Force filter to user's own keys
		targetUsername = user.Username
	}
	// Admin: if no username specified (empty string), search all users

	// Validate sort parameters
	if req.Sort.By != "" && !ValidSortFields[req.Sort.By] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid sort.by: must be one of: created_at, expires_at, last_used_at, name",
		})
		return
	}

	// Normalize and validate sort order (case-insensitive)
	if req.Sort.Order != "" {
		orderLower := strings.ToLower(strings.TrimSpace(req.Sort.Order))
		if !ValidSortOrders[orderLower] {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid sort.order: must be asc or desc",
			})
			return
		}
		req.Sort.Order = orderLower
	}

	// Validate pagination
	if req.Pagination.Limit < 1 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "pagination.limit must be at least 1",
		})
		return
	}
	if req.Pagination.Limit > MaxLimit {
		// Silently cap at maximum (user-friendly)
		req.Pagination.Limit = MaxLimit
	}
	if req.Pagination.Offset < 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "pagination.offset must be non-negative",
		})
		return
	}

	// Call service layer
	result, err := h.service.Search(
		c.Request.Context(),
		targetUsername,
		req.Filters,
		req.Sort,
		req.Pagination,
	)
	if err != nil {
		h.logger.Error("Failed to search API keys",
			"error", err,
			"username", targetUsername,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search API keys"})
		return
	}

	// Build response
	response := SearchAPIKeysResponse{
		Object:  "list",
		Data:    result.Keys,
		HasMore: result.HasMore,
	}

	c.JSON(http.StatusOK, response)
}

// BulkRevokeAPIKeys handles POST /v1/api-keys/bulk-revoke
// Revokes all active API keys for a specific user.
func (h *Handler) BulkRevokeAPIKeys(c *gin.Context) {
	var req BulkRevokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Authorization: users can revoke own keys, admins can revoke any user's keys
	if req.Username != user.Username && !h.isAdmin(c.Request.Context(), user) {
		h.logger.Warn("Unauthorized bulk revoke attempt",
			"requestingUser", user.Username,
			"targetUser", req.Username,
		)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Access denied: you can only bulk revoke your own API keys",
		})
		return
	}

	// Perform bulk revocation
	count, err := h.service.BulkRevokeAPIKeys(c.Request.Context(), req.Username)
	if err != nil {
		h.logger.Error("Failed to bulk revoke API keys",
			"error", err,
			"targetUser", req.Username,
			"requestingUser", user.Username,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke API keys"})
		return
	}

	h.logger.Info("Bulk revoked API keys",
		"count", count,
		"targetUser", req.Username,
		"revokedBy", user.Username,
	)

	response := BulkRevokeResponse{
		RevokedCount: count,
		Message:      fmt.Sprintf("Successfully revoked %d active API key(s) for user %s", count, req.Username),
	}

	c.JSON(http.StatusOK, response)
}
