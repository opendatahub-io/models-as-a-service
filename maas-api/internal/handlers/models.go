package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ModelsHandler handles model-related endpoints.
type ModelsHandler struct {
	modelMgr *models.Manager
	logger   *logger.Logger
}

// NewModelsHandler creates a new models handler.
func NewModelsHandler(log *logger.Logger, modelMgr *models.Manager) *ModelsHandler {
	if log == nil {
		log = logger.Production()
	}
	return &ModelsHandler{
		modelMgr: modelMgr,
		logger:   log,
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

	// Pass the API key directly to model discovery
	// Authorization is handled at gateway level (Authorino)
	modelList, err := h.modelMgr.ListAvailableLLMs(c.Request.Context(), bearerToken)
	if err != nil {
		claims, claimsErr := token.ExtractClaims(bearerToken)
		if claimsErr != nil {
			h.logger.Warn("Failed to extract claims from token", "error", claimsErr)
		}
		h.logger.Error("Failed to get available LLM models",
			"error", err,
			"jti", claims["jti"],
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
