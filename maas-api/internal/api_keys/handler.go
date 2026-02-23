package api_keys

import (
	"errors"
	"net/http"
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

func (h *Handler) CreateAPIKey(c *gin.Context) {
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

	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return
	}

	expiration := req.Expiration.Duration
	if err := token.ValidateExpiration(expiration, 10*time.Minute); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tok, err := h.service.CreateAPIKey(c.Request.Context(), user, req.Name, req.Description, expiration)
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
	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
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
	// TODO(production): Add authorization check - verify user owns key or is admin
	// Currently any authenticated user can view any key by ID
	tokenID := c.Param("id")
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Token ID required"})
		return
	}

	tok, err := h.service.GetAPIKey(c.Request.Context(), tokenID)
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to get API key",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
		return
	}

	c.JSON(http.StatusOK, tok)
}

// RevokeAllTokens handles DELETE /v1/tokens.
func (h *Handler) RevokeAllTokens(c *gin.Context) {
	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
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

// CreatePermanentKeyRequest is the request body for creating a permanent API key
type CreatePermanentKeyRequest struct {
	Name        string          `json:"name" binding:"required"`
	Description string          `json:"description,omitempty"`
	ExpiresIn   *token.Duration `json:"expiresIn,omitempty"`
}

// CreatePermanentAPIKey handles POST /v1/api-keys/permanent
// Creates a new API key (sk-oai-* format) per Feature Refinement
// Per "Keys Shown Only Once": key is returned ONCE at creation and never again
//
// TODO(post-POC): After removing SA-backed endpoint, consolidate to POST /v1/api-keys
// and deprecate /permanent suffix. Will support both permanent and expiring keys via
// optional expiresAt field in request body.
func (h *Handler) CreatePermanentAPIKey(c *gin.Context) {
	var req CreatePermanentKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return
	}

	// Parse expiration duration if provided
	var expiresIn *time.Duration
	if req.ExpiresIn != nil {
		d := req.ExpiresIn.Duration
		expiresIn = &d
	}

	result, err := h.service.CreatePermanentAPIKey(c.Request.Context(), user, req.Name, req.Description, expiresIn)
	if err != nil {
		h.logger.Error("Failed to create permanent API key", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.logger.Info("Created permanent API key",
		"keyId", result.ID,
		"keyPrefix", result.KeyPrefix,
	)

	// Return the key - THIS IS THE ONLY TIME THE PLAINTEXT IS SHOWN
	c.JSON(http.StatusCreated, result)
}

// ValidateAPIKeyRequest is the request body for validating an API key
type ValidateAPIKeyRequest struct {
	Key string `json:"key" binding:"required"`
}

// ValidateAPIKeyHandler handles POST /internal/v1/api-keys/validate
// This endpoint is called by Authorino via HTTP external auth callback
// Per Feature Refinement "Gateway Integration (Inference Flow)"
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
		// Return 401 with reason for invalid keys
		c.JSON(http.StatusUnauthorized, result)
		return
	}

	// Valid key - return user identity for Authorino to use
	c.JSON(http.StatusOK, result)
}

// RevokeAPIKey handles DELETE /v1/api-keys/:id
// Revokes a specific permanent API key (soft delete)
func (h *Handler) RevokeAPIKey(c *gin.Context) {
	// TODO(production): Add authorization check - verify user owns key or is admin
	// Currently any authenticated user can revoke any key by ID
	keyID := c.Param("id")
	if keyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API key ID required"})
		return
	}

	if err := h.service.RevokeAPIKey(c.Request.Context(), keyID); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to revoke API key", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke API key"})
		return
	}

	h.logger.Info("Revoked API key", "keyId", keyID)
	c.Status(http.StatusNoContent)
}
