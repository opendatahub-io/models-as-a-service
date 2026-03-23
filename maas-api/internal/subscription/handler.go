package subscription

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// Handler handles subscription selection requests.
type Handler struct {
	selector *Selector
	logger   *logger.Logger
}

// NewHandler creates a new subscription handler.
func NewHandler(log *logger.Logger, selector *Selector) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		selector: selector,
		logger:   log,
	}
}

// SelectSubscription handles POST /internal/v1/subscriptions/select requests.
//
// This endpoint is called by Authorino during AuthPolicy evaluation to determine
// which subscription a user should be assigned to. The request contains authenticated
// user information (groups, username) from auth.identity and an optional explicit
// subscription name from the X-MaaS-Subscription header.
//
// Selection logic:
//  1. If requestedSubscription is provided, validate user has access and return it
//  2. Otherwise, if user belongs to only one subscription, return it
//  3. If user belongs to multiple subscriptions, require explicit selection via header
//
// This endpoint is protected by NetworkPolicy and should only be accessible from
// Authorino pods. No additional authentication is needed as the groups/username
// come from an already-authenticated auth.identity object.
func (h *Handler) SelectSubscription(c *gin.Context) {
	h.logger.Debug("Subscription selection request received",
		"path", c.Request.URL.Path,
		"method", c.Request.Method,
	)

	var req SelectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.logger.Warn("Invalid request body",
			"error", err.Error(),
		)
		c.JSON(http.StatusOK, SelectResponse{
			Error:   "bad_request",
			Message: "invalid request body: " + err.Error(),
		})
		return
	}

	h.logger.Debug("Processing subscription selection",
		"username", req.Username,
		"groups", req.Groups,
		"requestedSubscription", req.RequestedSubscription,
	)

	response, err := h.selector.Select(req.Groups, req.Username, req.RequestedSubscription)
	if err != nil {
		var noSubErr *NoSubscriptionError
		var notFoundErr *SubscriptionNotFoundError
		var accessDeniedErr *AccessDeniedError
		var multipleSubsErr *MultipleSubscriptionsError

		if errors.As(err, &noSubErr) {
			h.logger.Debug("No subscription found for user",
				"username", req.Username,
				"groups", req.Groups,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "not_found",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &notFoundErr) {
			h.logger.Debug("Requested subscription not found",
				"subscription", req.RequestedSubscription,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "not_found",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &accessDeniedErr) {
			h.logger.Debug("Access denied to subscription",
				"username", req.Username,
				"subscription", req.RequestedSubscription,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "access_denied",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &multipleSubsErr) {
			h.logger.Debug("Multiple subscriptions found, explicit selection required",
				"username", req.Username,
				"subscriptions", multipleSubsErr.Subscriptions,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "multiple_subscriptions",
				Message: err.Error(),
			})
			return
		}

		// All other errors are internal server errors
		h.logger.Error("Subscription selection failed",
			"error", err.Error(),
			"username", req.Username,
		)
		c.JSON(http.StatusOK, SelectResponse{
			Error:   "internal_error",
			Message: "failed to select subscription: " + err.Error(),
		})
		return
	}

	h.logger.Debug("Subscription selected successfully",
		"username", req.Username,
		"subscription", response.Name,
		"organizationId", response.OrganizationID,
	)
	c.JSON(http.StatusOK, response)
}

// ListSubscriptions handles GET /v1/subscriptions.
// Returns all subscriptions the authenticated user has access to.
func (h *Handler) ListSubscriptions(c *gin.Context) {
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

	accessible, err := h.selector.GetAllAccessible(userContext.Groups, userContext.Username)
	if err != nil {
		h.logger.Error("Failed to list subscriptions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Failed to list subscriptions",
				"type":    "server_error",
			}})
		return
	}

	subs := make([]SubscriptionInfo, len(accessible))
	for i, sub := range accessible {
		subs[i] = ResponseToSubscriptionInfo(sub)
	}

	c.JSON(http.StatusOK, subs)
}

// ListSubscriptionsForModel handles GET /v1/model/:model-id/subscriptions.
// Returns subscriptions the user has access to that include the specified model.
func (h *Handler) ListSubscriptionsForModel(c *gin.Context) {
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

	modelID := c.Param("model-id")
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "model-id is required",
				"type":    "invalid_request_error",
			}})
		return
	}

	subs, err := h.selector.ListAccessibleForModel(userContext.Username, userContext.Groups, modelID)
	if err != nil {
		h.logger.Error("Failed to list subscriptions for model", "error", err, "model", modelID)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Failed to list subscriptions",
				"type":    "server_error",
			}})
		return
	}

	c.JSON(http.StatusOK, subs)
}
