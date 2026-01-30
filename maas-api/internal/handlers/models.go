package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ModelsHandler handles model-related endpoints.
type ModelsHandler struct {
	modelMgr     *models.Manager
	tokenManager *token.Manager
	logger       *logger.Logger
}

// NewModelsHandler creates a new models handler.
func NewModelsHandler(log *logger.Logger, modelMgr *models.Manager, tokenMgr *token.Manager) *ModelsHandler {
	if log == nil {
		log = logger.Production()
	}
	return &ModelsHandler{
		modelMgr:     modelMgr,
		tokenManager: tokenMgr,
		logger:       log,
	}
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Extract authorization token from header
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		h.logger.Error("Authorization header missing")
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Authorization token required",
				"type":    "authentication_error",
			}})
		return
	}

	bearerToken := strings.TrimSpace(authHeader)
	bearerToken, _ = strings.CutPrefix(bearerToken, "Bearer ")
	bearerToken = strings.TrimSpace(bearerToken)

	if bearerToken == "" {
		h.logger.Error("Empty token after processing authorization header")
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Valid authorization token required",
				"type":    "authentication_error",
			}})
		return
	}

	// Check if the token has the correct audience for model authorization checks.
	// If not, exchange it for a proper service account token.
	saToken := bearerToken
	if !h.tokenManager.HasValidAudience(bearerToken) {
		userCtx, exists := c.Get("user")
		if !exists {
			h.logger.Error("User context not found for token exchange")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}

		user, ok := userCtx.(*token.UserContext)
		if !ok {
			h.logger.Error("Invalid user context type")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}

		exchangedToken, err := h.tokenManager.GenerateToken(c.Request.Context(), user, 10*time.Minute)
		if err != nil {
			h.logger.Error("Token exchange failed",
				"error", err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to authorize request",
					"type":    "server_error",
				}})
			return
		}

		h.logger.Debug("Exchanged token for SA token with correct audience")
		saToken = exchangedToken.Token
	}

	modelList, err := h.modelMgr.ListAvailableLLMs(c.Request.Context(), saToken)
	if err != nil {
		h.logger.Error("Failed to get available LLM models",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Failed to retrieve LLM models",
				"type":    "server_error",
			}})
		return
	}

	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}
