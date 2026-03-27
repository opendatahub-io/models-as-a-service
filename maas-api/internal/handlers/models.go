package handlers

import (
	"errors"
	"net/http"
	"sort"
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
	subscriptionSelector *subscription.Selector
	logger               *logger.Logger
	maasModelRefLister   models.MaaSModelRefLister
}

// NewModelsHandler creates a new models handler.
// GET /v1/models lists models from the MaaSModelRef lister when set; otherwise the list is empty.
func NewModelsHandler(
	log *logger.Logger,
	modelMgr *models.Manager,
	subscriptionSelector *subscription.Selector,
	maasModelRefLister models.MaaSModelRefLister,
) *ModelsHandler {
	if log == nil {
		log = logger.Production()
	}
	return &ModelsHandler{
		modelMgr:             modelMgr,
		subscriptionSelector: subscriptionSelector,
		logger:               log,
		maasModelRefLister:   maasModelRefLister,
	}
}

// selectSubscriptionsForListing determines which subscriptions to use for model listing.
// Returns the subscriptions list and a shouldReturn flag (true if the handler should return early).
func (h *ModelsHandler) selectSubscriptionsForListing(
	c *gin.Context,
	userContext *token.UserContext,
	requestedSubscription string,
	returnAllModels bool,
) ([]*subscription.SelectResponse, bool) {
	if returnAllModels {
		// Return all models across all accessible subscriptions
		if h.subscriptionSelector != nil {
			allSubs, err := h.subscriptionSelector.GetAllAccessible(userContext.Groups, userContext.Username)
			if err != nil {
				h.logger.Error("Failed to get all accessible subscriptions", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"message": "Failed to get subscriptions",
						"type":    "server_error",
					}})
				return nil, true
			}
			h.logger.Debug("Returning models from all accessible subscriptions", "subscriptionCount", len(allSubs))
			return allSubs, false
		}
		// No selector configured - cannot return all models
		h.logger.Debug("X-MaaS-Return-All-Models requested but subscription selector not configured")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "X-MaaS-Return-All-Models not supported",
				"type":    "invalid_request_error",
			}})
		return nil, true
	}

	// Single subscription selection (existing behavior)
	if h.subscriptionSelector != nil {
		result, err := h.subscriptionSelector.Select(userContext.Groups, userContext.Username, requestedSubscription)
		if err != nil {
			h.handleSubscriptionSelectionError(c, err)
			return nil, true
		}
		return []*subscription.SelectResponse{result}, false
	}

	// If no selector configured and no subscription header provided, return empty
	// (don't create synthetic subscription metadata)
	if requestedSubscription == "" {
		return nil, false
	}

	// Use the requested subscription header as-is (for backward compat with legacy deployments)
	return []*subscription.SelectResponse{{Name: requestedSubscription}}, false
}

// handleSubscriptionSelectionError handles errors from subscription selection and sends appropriate HTTP responses.
func (h *ModelsHandler) handleSubscriptionSelectionError(c *gin.Context, err error) {
	var multipleSubsErr *subscription.MultipleSubscriptionsError
	var accessDeniedErr *subscription.AccessDeniedError
	var notFoundErr *subscription.SubscriptionNotFoundError
	var noSubErr *subscription.NoSubscriptionError

	// For consistency with inferencing (which uses Authorino and returns 403 for all
	// subscription errors), we return 403 Forbidden for all subscription-related errors.
	if errors.As(err, &multipleSubsErr) {
		h.logger.Debug("User has multiple subscriptions, x-maas-subscription header required",
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
		h.logger.Debug("Access denied to subscription")
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"message": err.Error(),
				"type":    "permission_error",
			}})
		return
	}

	if errors.As(err, &notFoundErr) {
		h.logger.Debug("Subscription not found")
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"message": err.Error(),
				"type":    "permission_error",
			}})
		return
	}

	if errors.As(err, &noSubErr) {
		h.logger.Debug("No subscription found for user")
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"message": err.Error(),
				"type":    "permission_error",
			}})
		return
	}

	// Other errors are internal server errors
	h.logger.Error("Subscription selection failed", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": gin.H{
			"message": "Failed to select subscription",
			"type":    "server_error",
		}})
}

// addSubscriptionIfNew adds a subscription to the model's subscriptions array if not already present.
func (h *ModelsHandler) addSubscriptionIfNew(model *models.Model, subInfo models.SubscriptionInfo) {
	for _, existingSub := range model.Subscriptions {
		if existingSub.Name == subInfo.Name {
			return
		}
	}
	model.Subscriptions = append(model.Subscriptions, subInfo)
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Require Authorization header and pass it through as-is to list and access validation.
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

	// Extract x-maas-subscription header to pass through to model endpoints for authorization checks.
	// This is required for users with multiple subscriptions.
	requestedSubscription := strings.TrimSpace(c.GetHeader("x-maas-subscription"))

	// Extract x-maas-return-all-models header to return models from all accessible subscriptions.
	returnAllModels := strings.ToLower(strings.TrimSpace(c.GetHeader("x-maas-return-all-models"))) == "true"

	// Validate header combination - cannot specify both
	if requestedSubscription != "" && returnAllModels {
		h.logger.Debug("Invalid request: both X-MaaS-Subscription and X-MaaS-Return-All-Models headers provided")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "Cannot specify both X-MaaS-Subscription and X-MaaS-Return-All-Models headers",
				"type":    "invalid_request_error",
			}})
		return
	}

	// Get user context for subscription selection
	var userContext *token.UserContext
	if h.subscriptionSelector != nil {
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
		var ok bool
		userContext, ok = userContextVal.(*token.UserContext)
		if !ok {
			h.logger.Error("Invalid user context type")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}
	}

	// Determine which subscriptions to use for model filtering
	subscriptionsToUse, shouldReturn := h.selectSubscriptionsForListing(c, userContext, requestedSubscription, returnAllModels)
	if shouldReturn {
		return
	}

	// Initialize to empty slice (not nil) so JSON marshals as [] instead of null
	modelList := []models.Model{}
	if h.maasModelRefLister != nil {
		h.logger.Debug("Listing models from MaaSModelRef cache (all namespaces)")
		list, err := models.ListFromMaaSModelRefLister(h.maasModelRefLister)
		if err != nil {
			h.logger.Error("Listing from MaaSModelRef failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Failed to list models",
					"type":    "server_error",
				}})
			return
		}

		// Distinguish between "no subscription system" and "user has zero subscriptions"
		if len(subscriptionsToUse) == 0 {
			if h.subscriptionSelector == nil {
				// Legacy case: no subscription system configured
				h.logger.Debug("No subscription system configured, filtering models without subscription header")
				modelList = h.modelMgr.FilterModelsByAccess(c.Request.Context(), list, authHeader, "")
			} else {
				// User has zero accessible subscriptions - return empty list
				h.logger.Debug("User has zero accessible subscriptions, returning empty model list")
				// modelList is already initialized to empty slice at line 235
			}
		} else {
			// Filter models by subscription(s) and aggregate subscriptions
			// Deduplication key is (model ID, URL) - models with the same ID and URL are
			// the same instance and should have their subscriptions aggregated into an array.
			type modelKey struct {
				id  string
				url string
			}
			modelsByKey := make(map[modelKey]*models.Model)

			for _, sub := range subscriptionsToUse {
				h.logger.Debug("Filtering models by subscription", "subscription", sub.Name, "modelCount", len(list))
				filteredModels := h.modelMgr.FilterModelsByAccess(c.Request.Context(), list, authHeader, sub.Name)

				for _, model := range filteredModels {
					subInfo := models.SubscriptionInfo{
						Name:        sub.Name,
						DisplayName: sub.DisplayName,
						Description: sub.Description,
					}

					// Create key from model ID and URL
					urlStr := ""
					if model.URL != nil {
						urlStr = model.URL.String()
					}
					key := modelKey{id: model.ID, url: urlStr}

					if existingModel, exists := modelsByKey[key]; exists {
						// Model already exists - append subscription if not already present
						h.addSubscriptionIfNew(existingModel, subInfo)
					} else {
						// New model - create entry with subscriptions array
						model.Subscriptions = []models.SubscriptionInfo{subInfo}
						modelsByKey[key] = &model
					}
				}
			}

			// Convert map to slice with deterministic ordering
			keys := make([]modelKey, 0, len(modelsByKey))
			for k := range modelsByKey {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				if keys[i].id != keys[j].id {
					return keys[i].id < keys[j].id
				}
				return keys[i].url < keys[j].url
			})
			for _, k := range keys {
				modelList = append(modelList, *modelsByKey[k])
			}
		}

		h.logger.Debug("Access validation complete", "listed", len(list), "accessible", len(modelList), "subscriptions", len(subscriptionsToUse))
	} else {
		h.logger.Debug("MaaSModelRef lister not configured, returning empty model list")
	}

	h.logger.Debug("GET /v1/models returning models", "count", len(modelList))
	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}
