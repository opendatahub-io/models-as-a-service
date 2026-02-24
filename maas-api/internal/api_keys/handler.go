package api_keys

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

type Handler struct {
	service *Service
	logger  *logger.Logger
}

func NewHandler(log *logger.Logger, service *Service) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		service: service,
		logger:  log,
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

// isAuthorizedForKey checks if the user is authorized to access the API key.
// User is authorized if they own the key or are an admin.
func (h *Handler) isAuthorizedForKey(user *token.UserContext, keyOwner string) bool {
	// Check if user owns the key
	if user.Username == keyOwner {
		return true
	}

	// Check if user is admin (has admin-users group)
	return slices.Contains(user.Groups, "admin-users")
}

type CreateRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Expiration  *token.Duration `json:"expiration"`
}

type Response struct {
	Token       string `json:"token"`
	Expiration  string `json:"expiration"`
	ExpiresAt   int64  `json:"expiresAt"`
	JTI         string `json:"jti"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CreateServiceAccountAPIKey creates a K8s ServiceAccount-based API key (legacy, will be removed in future).
func (h *Handler) CreateServiceAccountAPIKey(c *gin.Context) {
	var req CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token name is required for api keys"})
		return
	}

	if req.Expiration == nil {
		req.Expiration = &token.Duration{Duration: time.Hour * 24 * 30} // Default to 30 days
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}

	expiration := req.Expiration.Duration
	if err := token.ValidateExpiration(expiration, 10*time.Minute); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tok, err := h.service.CreateServiceAccountAPIKey(c.Request.Context(), user, req.Name, req.Description, expiration)
	if err != nil {
		h.logger.Error("Failed to generate API key",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate api key"})
		return
	}

	c.JSON(http.StatusCreated, Response{
		Token:       tok.Token.Token,
		Expiration:  tok.Expiration.String(),
		ExpiresAt:   tok.ExpiresAt,
		JTI:         tok.JTI,
		Name:        tok.Name,
		Description: tok.Description,
	})
}

func (h *Handler) ListAPIKeys(c *gin.Context) {
	user := h.getUserContext(c)
	if user == nil {
		return
	}

	tokens, err := h.service.ListAPIKeys(c.Request.Context(), user)
	if err != nil {
		h.logger.Error("Failed to list API keys",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list api keys"})
		return
	}

	c.JSON(http.StatusOK, tokens)
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
	if !h.isAuthorizedForKey(user, tok.Username) {
		h.logger.Warn("Unauthorized API key access attempt",
			"requestingUser", user.Username,
			"keyOwner", tok.Username,
			"keyId", tokenID,
		)
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied: you can only view your own API keys"})
		return
	}

	c.JSON(http.StatusOK, tok)
}

// RevokeAllTokens handles DELETE /v1/tokens.
func (h *Handler) RevokeAllTokens(c *gin.Context) {
	user := h.getUserContext(c)
	if user == nil {
		return
	}

	if err := h.service.RevokeAll(c.Request.Context(), user); err != nil {
		h.logger.Error("Failed to revoke tokens",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke tokens"})
		return
	}

	h.logger.Debug("Successfully revoked tokens")
	c.Status(http.StatusNoContent)
}

// CreateAPIKeyRequest is the request body for creating an API key.
// Keys can be permanent (no expiresIn) or expiring (with expiresIn).
type CreateAPIKeyRequest struct {
	Name        string          `binding:"required"           json:"name"`
	Description string          `json:"description,omitempty"`
	ExpiresIn   *token.Duration `json:"expiresIn,omitempty"` // Optional - nil means permanent
}

// CreateAPIKey handles POST /v2/api-keys
// Creates a new API key (sk-oai-* format) per Feature Refinement.
// Keys can be permanent (no expiresIn) or expiring (with expiresIn).
// Per "Keys Shown Only Once": key is returned ONCE at creation and never again.
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

	// Parse expiration duration if provided
	var expiresIn *time.Duration
	if req.ExpiresIn != nil {
		d := req.ExpiresIn.Duration
		expiresIn = &d
	}

	result, err := h.service.CreateAPIKey(c.Request.Context(), user, req.Name, req.Description, expiresIn)
	if err != nil {
		h.logger.Error("Failed to create API key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.logger.Info("Created API key",
		"keyId", result.ID,
		"keyPrefix", result.KeyPrefix,
	)

	// Return the key - THIS IS THE ONLY TIME THE PLAINTEXT IS SHOWN
	c.JSON(http.StatusCreated, result)
}

// ValidateAPIKeyRequest is the request body for validating an API key.
type ValidateAPIKeyRequest struct {
	Key string `binding:"required" json:"key"`
}

// ValidateAPIKeyHandler handles POST /internal/v2/api-keys/validate
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
// Revokes a specific permanent API key (soft delete).
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
	if !h.isAuthorizedForKey(user, keyMetadata.Username) {
		h.logger.Warn("Unauthorized API key revocation attempt",
			"requestingUser", user.Username,
			"keyOwner", keyMetadata.Username,
			"keyId", keyID,
		)
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied: you can only revoke your own API keys"})
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
	c.Status(http.StatusNoContent)
}
