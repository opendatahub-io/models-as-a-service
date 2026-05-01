package handlers

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

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
		// User token authentication - return all models across all accessible subscriptions
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
			h.logger.Debug("User token - returning models from all accessible subscriptions", "subscriptionCount", len(allSubs))
			return allSubs, false
		}
		// No selector configured - cannot return all models
		h.logger.Debug("Subscription selector not configured")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Subscription system not configured",
				"type":    "server_error",
			}})
		return nil, true
	}

	// API key authentication - filter by the subscription bound to the key
	if h.subscriptionSelector != nil {
		//nolint:unqueryvet,nolintlint // Select is a method, not a SQL query
		result, err := h.subscriptionSelector.Select(userContext.Groups, userContext.Username, requestedSubscription, "")
		if err != nil {
			h.handleSubscriptionSelectionError(c, err)
			return nil, true
		}
		h.logger.Debug("API key - filtering by subscription", "subscription", result.Name)
		return []*subscription.SelectResponse{result}, false
	}

	// If no selector configured and no subscription header provided, return empty
	// (don't create synthetic subscription metadata)
	if requestedSubscription == "" {
		return nil, false
	}

	// Use the requested subscription header as-is (for legacy deployments without subscription selector)
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
		// This should not happen with API keys (subscription is bound at mint time)
		// If it does, it indicates the API key was minted without a subscription
		h.logger.Debug("API key has no subscription bound - invalid state",
			"subscriptionCount", len(multipleSubsErr.Subscriptions),
		)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"message": "API key has no subscription bound",
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

// extractAndValidateAuth validates and extracts authentication details.
// Returns authHeader, requestedSubscription, isAPIKeyRequest, and error.
func (h *ModelsHandler) extractAndValidateAuth(c *gin.Context) (string, string, bool, error) {
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	if authHeader == "" {
		h.logger.Error("Authorization header missing")
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Authorization required",
				"type":    "authentication_error",
			}})
		return "", "", false, errors.New("missing authorization")
	}

	// Extract x-maas-subscription header.
	requestedSubscription := ""
	headerValues := c.Request.Header.Values("X-Maas-Subscription")
	for i := len(headerValues) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(headerValues[i])
		if trimmed != "" {
			requestedSubscription = trimmed
			break
		}
	}
	isAPIKeyRequest := strings.HasPrefix(authHeader, "Bearer sk-oai-")

	// Fail closed: API keys without a bound subscription must be rejected
	if isAPIKeyRequest && requestedSubscription == "" {
		h.logger.Debug("API key request missing bound subscription header")
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"message": "API key has no subscription bound",
				"type":    "permission_error",
			}})
		return "", "", false, errors.New("api key missing subscription")
	}

	return authHeader, requestedSubscription, isAPIKeyRequest, nil
}

// getUserContextIfNeeded retrieves user context from the request if subscription selector is configured.
func (h *ModelsHandler) getUserContextIfNeeded(c *gin.Context) (*token.UserContext, error) {
	if h.subscriptionSelector == nil {
		return nil, nil
	}

	userContextVal, exists := c.Get("user")
	if !exists {
		h.logger.Error("User context not found - ExtractUserInfo middleware not called")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Internal server error",
				"type":    "server_error",
			}})
		return nil, errors.New("user context not found")
	}

	userContext, ok := userContextVal.(*token.UserContext)
	if !ok {
		h.logger.Error("Invalid user context type")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Internal server error",
				"type":    "server_error",
			}})
		return nil, errors.New("invalid user context type")
	}

	return userContext, nil
}

// aggregateModelsFromSubscriptions filters and aggregates models across multiple subscriptions.
func (h *ModelsHandler) aggregateModelsFromSubscriptions(
	c *gin.Context,
	list []models.Model,
	subscriptionsToUse []*subscription.SelectResponse,
	authHeader string,
) []models.Model {
	type modelKey struct {
		id      string
		url     string
		ownedBy string
	}
	modelsByKey := make(map[modelKey]*models.Model)

	for _, sub := range subscriptionsToUse {
		// Pre-filter by modelRefs if available (optimization to reduce HTTP calls)
		modelsToCheck := list
		if len(sub.ModelRefs) > 0 {
			h.logger.Debug("Pre-filtering models by subscription modelRefs",
				"subscription", sub.Name,
				"totalModels", len(list),
				"modelRefsCount", len(sub.ModelRefs),
			)
			modelsToCheck = filterModelsBySubscription(list, sub.ModelRefs)
			h.logger.Debug("After modelRef filtering", "modelsToCheck", len(modelsToCheck))
		}

		probeSubscriptionHeader := sub.Name
		h.logger.Debug("Filtering models by subscription", "subscription", sub.Name, "modelCount", len(modelsToCheck), "probeWithSubscriptionHeader", probeSubscriptionHeader != "")
		filteredModels := h.modelMgr.FilterModelsByAccess(c.Request.Context(), modelsToCheck, authHeader, probeSubscriptionHeader)

		for _, model := range filteredModels {
			subInfo := models.SubscriptionInfo{
				Name:        sub.Name,
				DisplayName: sub.DisplayName,
				Description: sub.Description,
			}

			urlStr := ""
			if model.URL != nil {
				urlStr = model.URL.String()
			}
			key := modelKey{id: model.ID, url: urlStr, ownedBy: model.OwnedBy}

			if existingModel, exists := modelsByKey[key]; exists {
				h.addSubscriptionIfNew(existingModel, subInfo)
			} else {
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
		if keys[i].url != keys[j].url {
			return keys[i].url < keys[j].url
		}
		return keys[i].ownedBy < keys[j].ownedBy
	})

	modelList := make([]models.Model, 0, len(keys))
	for _, k := range keys {
		modelList = append(modelList, *modelsByKey[k])
	}
	return modelList
}

// ListLLMs handles GET /v1/models.
func (h *ModelsHandler) ListLLMs(c *gin.Context) {
	// Validate and extract authentication details
	authHeader, requestedSubscription, isAPIKeyRequest, err := h.extractAndValidateAuth(c)
	if err != nil {
		return
	}

	// Determine behavior based on auth method
	returnAllModels := !isAPIKeyRequest && requestedSubscription == ""

	// Get user context for subscription selection
	userContext, err := h.getUserContextIfNeeded(c)
	if err != nil {
		return
	}

	// Log the authentication method and filtering behavior
	if requestedSubscription != "" {
		h.logger.Debug("API key request - filtering models by subscription",
			"subscription", requestedSubscription,
		)
	} else {
		h.logger.Debug("User token request - returning all accessible models")
	}

	// Determine which subscriptions to use for model filtering
	subscriptionsToUse, shouldReturn := h.selectSubscriptionsForListing(c, userContext, requestedSubscription, returnAllModels)
	if shouldReturn {
		return
	}

	// Initialize to empty slice (not nil) so JSON marshals as [] instead of null
	modelList := []models.Model{}
	accessCheckedAt := time.Now().UTC()
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
				// modelList is already initialized to empty slice above
			}
		} else {
			// Filter models by subscription(s) and aggregate subscriptions
			modelList = h.aggregateModelsFromSubscriptions(c, list, subscriptionsToUse, authHeader)
		}

		accessCheckedAt = time.Now().UTC()
		h.logger.Debug("Access validation complete", "listed", len(list), "accessible", len(modelList), "subscriptions", len(subscriptionsToUse))
	} else {
		h.logger.Debug("MaaSModelRef lister not configured, returning empty model list")
	}

	// Prevent clients and proxies from caching authorization-checked model listings.
	// The access check is a point-in-time snapshot; auth policies may change at any moment.
	// X-Access-Checked-At lets clients assess the freshness of the authorization decision.
	c.Header("Cache-Control", "no-store")
	c.Header("X-Access-Checked-At", accessCheckedAt.Format(time.RFC3339))

	h.logger.Debug("GET /v1/models returning models", "count", len(modelList))
	c.JSON(http.StatusOK, pagination.Page[models.Model]{
		Object: "list",
		Data:   modelList,
	})
}

// filterModelsBySubscription filters models to only those matching the subscription's modelRefs.
func filterModelsBySubscription(modelList []models.Model, modelRefs []subscription.ModelRefInfo) []models.Model {
	if len(modelRefs) == 0 {
		return modelList
	}

	// Build map of allowed models for fast lookup
	allowed := make(map[string]bool)
	for _, ref := range modelRefs {
		key := ref.Namespace + "/" + ref.Name
		allowed[key] = true
	}

	// Filter models
	filtered := make([]models.Model, 0, len(modelList))
	for _, model := range modelList {
		// Models from MaaSModelRefLister have OwnedBy set to namespace/name
		modelKey := model.OwnedBy
		if allowed[modelKey] {
			filtered = append(filtered, model)
		}
	}

	return filtered
}
