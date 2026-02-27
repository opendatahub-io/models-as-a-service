package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ModelsHandler handles model-related endpoints.
type ModelsHandler struct {
	modelMgr             *models.Manager
	tokenManager         *token.Manager
	subscriptionSelector *subscription.Selector
	logger               *logger.Logger
	maasModelLister      models.MaaSModelLister
	maasModelNamespace   string
}

// NewModelsHandler creates a new models handler.
// GET /v1/models lists models from the MaaSModel lister when set; otherwise the list is empty.
func NewModelsHandler(
	log *logger.Logger,
	modelMgr *models.Manager,
	tokenMgr *token.Manager,
	subscriptionSelector *subscription.Selector,
	maasModelLister models.MaaSModelLister,
	maasModelNamespace string,
) *ModelsHandler {
	if log == nil {
		log = logger.Production()
	}
	return &ModelsHandler{
		modelMgr:             modelMgr,
		tokenManager:         tokenMgr,
		subscriptionSelector: subscriptionSelector,
		logger:               log,
		maasModelLister:      maasModelLister,
		maasModelNamespace:   maasModelNamespace,
	}
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Require Authorization header and pass it through as-is to list and access validation.
	// TODO: Once minting is done we may revisit token exchange (e.g. mint SA token for gateway auth when audience doesn't match).
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	if authHeader == "" {
		h.logger.Error("Authorization header missing")
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Authorization required",
				"type":    "authentication_error",
			}})
		return
	}

	// Extract user info from context (set by ExtractUserInfo middleware)
	userContextVal, exists := c.Get("user")
	if !exists {
		h.logger.Error("User context not found - ExtractUserInfo middleware not called")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Internal server error",
				"type":    "server_error",
			}})
		return
	}
	userContext, ok := userContextVal.(*token.UserContext)
	if !ok {
		h.logger.Error("Invalid user context type")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Internal server error",
				"type":    "server_error",
			}})
		return
	}

	// Extract x-maas-subscription header to pass through to model endpoints for authorization checks.
	// This is required for users with multiple subscriptions.
	requestedSubscription := strings.TrimSpace(c.GetHeader("x-maas-subscription"))

	// Validate subscription access before probing models.
	// This ensures consistent error handling with inferencing endpoints.
	var selectedSubscription string
	if h.subscriptionSelector != nil {
		result, err := h.subscriptionSelector.Select(userContext.Groups, userContext.Username, requestedSubscription)
		if err != nil {
			var multipleSubsErr *subscription.MultipleSubscriptionsError
			var accessDeniedErr *subscription.AccessDeniedError
			var notFoundErr *subscription.SubscriptionNotFoundError
			var noSubErr *subscription.NoSubscriptionError

			// For consistency with inferencing (which uses Authorino and returns 403 for all
			// subscription errors), we return 403 Forbidden for all subscription-related errors.
			if errors.As(err, &multipleSubsErr) {
				h.logger.Debug("User has multiple subscriptions, x-maas-subscription header required",
					"username", userContext.Username,
					"subscriptions", multipleSubsErr.Subscriptions,
				)
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &accessDeniedErr) {
				h.logger.Debug("Access denied to subscription",
					"username", userContext.Username,
					"subscription", selectedSubscription,
				)
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &notFoundErr) {
				h.logger.Debug("Subscription not found",
					"subscription", selectedSubscription,
				)
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &noSubErr) {
				h.logger.Debug("No subscription found for user",
					"username", userContext.Username,
				)
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			// Other errors are internal server errors
			h.logger.Error("Subscription selection failed",
				"error", err,
				"username", userContext.Username,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to select subscription",
					"type":    "server_error",
				}})
			return
		}
		// Use the selected subscription (which may be auto-selected if user only has one)
		selectedSubscription = result.Name
	} else {
		// If no selector configured, use the requested subscription header as-is
		selectedSubscription = requestedSubscription
	}

	var modelList []models.Model
	if h.maasModelLister != nil && h.maasModelNamespace != "" {
		h.logger.Debug("Listing models from MaaSModel cache", "namespace", h.maasModelNamespace)
		list, err := models.ListFromMaaSModelLister(h.maasModelLister, h.maasModelNamespace)
		if err != nil {
			h.logger.Error("Listing from MaaSModel failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to list models",
					"type":    "server_error",
				}})
			return
		}
		h.logger.Debug("MaaSModel list succeeded, validating access by probing each model endpoint", "modelCount", len(list), "subscription", selectedSubscription)
		modelList = h.modelMgr.FilterModelsByAccess(c.Request.Context(), list, authHeader, selectedSubscription)
		h.logger.Debug("Access validation complete", "listed", len(list), "accessible", len(modelList))
	} else {
		h.logger.Debug("MaaSModel not configured (lister or namespace unset), returning empty model list")
	}

	h.logger.Debug("GET /v1/models returning models", "count", len(modelList))
	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}
