package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/openai/openai-go/v2/packages/pagination"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ModelsHandler handles model-related endpoints.
type ModelsHandler struct {
	modelMgr             *models.Manager
	subscriptionSelector *subscription.Selector
	logger               logr.Logger
	maasModelRefLister   models.MaaSModelRefLister
}

// NewModelsHandler creates a new models handler.
// GET /v1/models lists models from the MaaSModelRef lister when set; otherwise the list is empty.
func NewModelsHandler(
	log logr.Logger,
	modelMgr *models.Manager,
	subscriptionSelector *subscription.Selector,
	maasModelRefLister models.MaaSModelRefLister,
) *ModelsHandler {
	return &ModelsHandler{
		modelMgr:             modelMgr,
		subscriptionSelector: subscriptionSelector,
		logger:               log,
		maasModelRefLister:   maasModelRefLister,
	}
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Require Authorization header and pass it through as-is to list and access validation.
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

	// Extract x-maas-subscription header to pass through to model endpoints for authorization checks.
	// This is required for users with multiple subscriptions.
	requestedSubscription := strings.TrimSpace(c.GetHeader("x-maas-subscription"))

	// Validate subscription access before probing models.
	// This ensures consistent error handling with inferencing endpoints.
	var selectedSubscription string
	if h.subscriptionSelector != nil {
		// Extract user info from context (set by ExtractUserInfo middleware)
		// Only needed when subscription selector is configured
		userContextVal, exists := c.Get("user")
		if !exists {
			h.logger.Error(nil, "User context not found - ExtractUserInfo middleware not called")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}
		userContext, ok := userContextVal.(*token.UserContext)
		if !ok {
			h.logger.Error(nil, "Invalid user context type")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}

		result, err := h.subscriptionSelector.Select(userContext.Groups, userContext.Username, requestedSubscription)
		if err != nil {
			var multipleSubsErr *subscription.MultipleSubscriptionsError
			var accessDeniedErr *subscription.AccessDeniedError
			var notFoundErr *subscription.SubscriptionNotFoundError
			var noSubErr *subscription.NoSubscriptionError

			// For consistency with inferencing (which uses Authorino and returns 403 for all
			// subscription errors), we return 403 Forbidden for all subscription-related errors.
			if errors.As(err, &multipleSubsErr) {
				h.logger.V(1).Info("User has multiple subscriptions, x-maas-subscription header required",
					"subscriptionCount", len(multipleSubsErr.Subscriptions),
				)
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &accessDeniedErr) {
				h.logger.V(1).Info("Access denied to subscription")
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &notFoundErr) {
				h.logger.V(1).Info("Subscription not found")
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			if errors.As(err, &noSubErr) {
				h.logger.V(1).Info("No subscription found for user")
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"message": err.Error(),
						"type":    "permission_error",
					}})
				return
			}

			// Other errors are internal server errors
			h.logger.Error(err, "Subscription selection failed")
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

	// Initialize to empty slice (not nil) so JSON marshals as [] instead of null
	modelList := []models.Model{}
	if h.maasModelRefLister != nil {
		h.logger.V(1).Info("Listing models from MaaSModelRef cache (all namespaces)")
		list, err := models.ListFromMaaSModelRefLister(h.maasModelRefLister)
		if err != nil {
			h.logger.Error(err, "Listing from MaaSModelRef failed")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to list models",
					"type":    "server_error",
				}})
			return
		}
		h.logger.V(1).Info("MaaSModelRef list succeeded, validating access by probing each model endpoint",
			"modelCount", len(list), "subscriptionHeaderProvided", selectedSubscription != "")
		modelList = h.modelMgr.FilterModelsByAccess(c.Request.Context(), list, authHeader, selectedSubscription)
		h.logger.V(1).Info("Access validation complete", "listed", len(list), "accessible", len(modelList))
	} else {
		h.logger.V(1).Info("MaaSModelRef lister not configured, returning empty model list")
	}

	h.logger.V(1).Info("GET /v1/models returning models", "count", len(modelList))
	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}
