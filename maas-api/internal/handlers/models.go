package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ModelsHandler handles model-related endpoints.
type ModelsHandler struct {
	modelMgr           *models.Manager
	tokenManager       *token.Manager
	logger             logr.Logger
	maasModelLister    models.MaaSModelLister
	maasModelNamespace string
}

// NewModelsHandler creates a new models handler.
// GET /v1/models lists models from the MaaSModel lister when set; otherwise the list is empty.
func NewModelsHandler(log logr.Logger, modelMgr *models.Manager, tokenMgr *token.Manager, maasModelLister models.MaaSModelLister, maasModelNamespace string) *ModelsHandler {
	return &ModelsHandler{
		modelMgr:           modelMgr,
		tokenManager:       tokenMgr,
		logger:             log,
		maasModelLister:    maasModelLister,
		maasModelNamespace: maasModelNamespace,
	}
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Require Authorization header and pass it through as-is to list and access validation.
	// TODO: Once minting is done we may revisit token exchange (e.g. mint SA token for gateway auth when audience doesn't match).
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	if authHeader == "" {
		h.logger.Error(nil, "Authorization header missing")
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Authorization required",
				"type":    "authentication_error",
			}})
		return
	}

	var modelList []models.Model
	if h.maasModelLister != nil && h.maasModelNamespace != "" {
		h.logger.V(1).Info("Listing models from MaaSModel cache", "namespace", h.maasModelNamespace)
		list, err := models.ListFromMaaSModelLister(h.maasModelLister, h.maasModelNamespace)
		if err != nil {
			h.logger.Error(err, "Listing from MaaSModel failed")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to list models",
					"type":    "server_error",
				}})
			return
		}
		h.logger.V(1).Info("MaaSModel list succeeded, validating access by probing each model endpoint", "modelCount", len(list))
		modelList = h.modelMgr.FilterModelsByAccess(c.Request.Context(), list, authHeader)
		h.logger.V(1).Info("Access validation complete", "listed", len(list), "accessible", len(modelList))
	} else {
		h.logger.V(1).Info("MaaSModel not configured (lister or namespace unset), returning empty model list")
	}

	h.logger.V(1).Info("GET /v1/models returning models", "count", len(modelList))
	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}
