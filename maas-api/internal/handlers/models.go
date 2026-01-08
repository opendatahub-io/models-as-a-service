package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
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

// ListModels handles GET /models.
func (h *ModelsHandler) ListModels(c *gin.Context) {
	modelList, err := h.modelMgr.ListAvailableModels()
	if err != nil {
		h.logger.Error("Failed to get available models",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve models"})
		return
	}

	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Extract service account token for authorization as recommended in PR feedback
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

	// Use strings.TrimSpace and strings.CutPrefix as suggested in PR feedback
	saToken := strings.TrimSpace(authHeader)
	saToken, hasBearerPrefix := strings.CutPrefix(saToken, "Bearer ")
	saToken = strings.TrimSpace(saToken)

	// Validate token is non-empty after processing
	if saToken == "" {
		h.logger.Error("Empty token after processing authorization header",
			"authHeader", authHeader,
			"hasBearerPrefix", hasBearerPrefix,
		)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Valid authorization token required",
				"type":    "authentication_error",
			}})
		return
	}

	modelList, err := h.modelMgr.ListAvailableLLMsForUser(c.Request.Context(), saToken)
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
