package subscription

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// MetricsRecorder is the subset of metrics.MetricsRecorder used by this handler.
type MetricsRecorder interface {
	RecordRejection(reason string)
}

// maxSelectRequestBodyBytes bounds the Authorino callback before JSON decoding.
// It matches the API-wide body limit: legacy selection can carry an unbounded
// provider group claim, so a smaller callback-only limit would reject tokens
// that existing gateway flows accept. The explicit bound still prevents an
// oversized body from being partially parsed or used to exhaust API memory.
const maxSelectRequestBodyBytes = 1 << 20

func (h *Handler) withContext(c *gin.Context) *Handler {
	requestHandler := *h
	if requestLogger := middleware.GetLogger(c); requestLogger != nil {
		requestHandler.logger = requestLogger
	} else {
		requestHandler.logger = h.logger.WithContext(c.Request.Context())
	}
	return &requestHandler
}

// Handler handles subscription selection requests.
type Handler struct {
	selector *Selector
	logger   *logger.Logger
	metrics  MetricsRecorder
}

// NewHandler creates a new subscription handler.
func NewHandler(log *logger.Logger, selector *Selector, metrics MetricsRecorder) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		selector: selector,
		logger:   log,
		metrics:  metrics,
	}
}

// SelectSubscription handles POST /internal/v1/subscriptions/select requests.
//
// This endpoint is called by Authorino during AuthPolicy evaluation. The
// default mode preserves the existing inference-selection contract. The
// subscription-admin mode is a separate authorization contract and cannot
// reuse an inference-selection success as a positive admin result.
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
	h = h.withContext(c)

	h.logger.Debug("Subscription selection request received",
		"path", c.Request.URL.Path,
		"method", c.Request.Method,
	)

	req, err := decodeSelectRequest(c)
	if err != nil {
		h.logger.Warn("Invalid request body",
			"error", err.Error(),
		)
		c.JSON(http.StatusOK, SelectResponse{
			Error:   "bad_request",
			Message: "invalid request body",
		})
		return
	}

	switch req.Purpose {
	// Absent keeps older Authorino request bodies on the legacy path; select
	// makes the same mode explicit for newly generated policies.
	case "", PurposeSelect:
		if req.Username == "" {
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "bad_request",
				Message: "username is required",
			})
			return
		}
		if len(req.Identity) != 0 {
			// Inference selection is based on the existing username/group
			// contract. Mixing the new identity envelope into that mode would
			// blur authentication data with a future admin grant.
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "bad_request",
				Message: "identity is not accepted for subscription selection",
			})
			return
		}
	case PurposeSubscriptionAdmin:
		h.handleSubscriptionAdmin(c, req)
		return
	default:
		// Never infer a callback mode from request shape. A typo or future mode
		// must deny rather than accidentally fall back to inference selection.
		c.JSON(http.StatusOK, SelectResponse{
			Error:   "bad_purpose",
			Message: "unknown callback purpose",
		})
		return
	}

	h.logger.Debug("Processing subscription selection",
		"username", logger.RedactValue(req.Username),
		"groups", req.Groups,
		"requestedSubscription", req.RequestedSubscription,
		"requestedModel", req.RequestedModel,
	)

	response, err := h.selector.SelectWithContext(c.Request.Context(), req.Groups, req.Username, req.RequestedSubscription, req.RequestedModel)
	if err != nil {
		// NOTE: All error responses return http.StatusOK with error fields populated in SelectResponse.
		// This is intentional for Authorino integration, which expects 200 OK responses with metadata
		// fields (not HTTP error codes). See SelectResponse type documentation in types.go.
		var noSubErr *NoSubscriptionError
		var notFoundErr *SubscriptionNotFoundError
		var accessDeniedErr *AccessDeniedError
		var multipleSubsErr *MultipleSubscriptionsError
		var modelNotInSubErr *ModelNotInSubscriptionError
		var modelUnhealthyErr *ModelUnhealthyError
		var guardrailErr *GuardrailResolutionError

		if errors.As(err, &noSubErr) {
			h.recordRejection(constant.RejectionNoCapacity)
			h.logger.Debug("No subscription found for user",
				"username", logger.RedactValue(req.Username),
				"groups", req.Groups,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "not_found",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &notFoundErr) {
			h.recordRejection(constant.RejectionNoCapacity)
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
			h.recordRejection(constant.RejectionUnauthorized)
			h.logger.Debug("Access denied to subscription",
				"username", logger.RedactValue(req.Username),
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
				"username", logger.RedactValue(req.Username),
				"subscriptions", multipleSubsErr.Subscriptions,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "multiple_subscriptions",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &modelNotInSubErr) {
			h.recordRejection(constant.RejectionNoCapacity)
			h.logger.Debug("Model not included in subscription",
				"subscription", modelNotInSubErr.Subscription,
				"model", modelNotInSubErr.Model,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "model_not_in_subscription",
				Message: err.Error(),
			})
			return
		}

		if errors.As(err, &modelUnhealthyErr) {
			h.recordUnhealthyRejection(modelUnhealthyErr)
			h.logger.Debug("Requested model is unhealthy",
				"subscription", modelUnhealthyErr.Subscription,
				"phase", modelUnhealthyErr.Phase,
				"reason", modelUnhealthyErr.Reason,
				"message", modelUnhealthyErr.Message,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "model_unhealthy",
				Message: modelUnhealthyErr.Message,
				Phase:   modelUnhealthyErr.Phase,
			})
			return
		}

		if errors.As(err, &guardrailErr) {
			h.recordRejection(constant.RejectionGuardrailUnavailable)
			h.logger.Error("Selected guardrails could not be resolved",
				"error", guardrailErr.Unwrap(),
				"subscription", req.RequestedSubscription,
				"model", req.RequestedModel,
			)
			c.JSON(http.StatusOK, SelectResponse{
				Error:   "guardrail_unavailable",
				Message: "selected guardrails are unavailable",
			})
			return
		}

		// All other errors are internal server errors
		h.logger.Error("Subscription selection failed",
			"error", err.Error(),
			"username", logger.RedactValue(req.Username),
		)
		c.JSON(http.StatusOK, SelectResponse{
			Error:   "internal_error",
			Message: "failed to select subscription: " + err.Error(),
		})
		return
	}

	h.logger.Debug("Subscription selected successfully",
		"username", logger.RedactValue(req.Username),
		"subscription", response.Name,
		"organizationId", response.OrganizationID,
	)
	token.SetOrganizationID(c, response.OrganizationID)
	c.JSON(http.StatusOK, response)
}

func decodeSelectRequest(c *gin.Context) (SelectRequest, error) {
	// DisallowUnknownFields makes callback evolution explicit: old APIs reject
	// new fields instead of silently ignoring data that a newer gateway expects
	// them to authorize. The trailing decode rejects concatenated JSON values.
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxSelectRequestBodyBytes))
	decoder.DisallowUnknownFields()

	var request SelectRequest
	if err := decoder.Decode(&request); err != nil {
		return SelectRequest{}, err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SelectRequest{}, errors.New("trailing data")
	}
	return request, nil
}

func (h *Handler) handleSubscriptionAdmin(c *gin.Context, request SelectRequest) {
	if request.Username != "" || len(request.Groups) != 0 || request.RequestedModel != "" {
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "bad_request"},
			Error:   "bad_request",
			Message: "legacy selection fields are not accepted for subscription-admin",
		})
		return
	}
	if len(request.Identity) == 0 {
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "invalid_identity"},
			Error:   "invalid_identity",
			Message: "trusted identity is required",
		})
		return
	}
	if len(request.Identity) > token.MaxIdentityEnvelopeBytes {
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "invalid_identity"},
			Error:   "invalid_identity",
			Message: "trusted identity is too large",
		})
		return
	}
	identity, err := token.ParseIdentityEnvelope(string(request.Identity))
	if err != nil {
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "invalid_identity"},
			Error:   "invalid_identity",
			Message: "trusted identity is invalid",
		})
		return
	}
	if request.RequestedSubscription == "" {
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "bad_request"},
			Error:   "bad_request",
			Message: "requestedSubscription is required",
		})
		return
	}

	scope, err := h.selector.ResolveSubscriptionAdmin(identity, request.RequestedSubscription)
	if err != nil {
		var adminErr *SubscriptionAdminError
		if errors.As(err, &adminErr) {
			c.JSON(http.StatusOK, SelectResponse{
				Purpose: PurposeSubscriptionAdmin,
				Admin:   &AdminDecision{Allowed: false, Reason: adminErr.Code},
				Error:   adminErr.Code,
				Message: adminErr.Message,
			})
			return
		}

		h.logger.Error("Subscription-admin resolution failed",
			"error", err.Error(),
			"requestedSubscription", request.RequestedSubscription,
		)
		c.JSON(http.StatusOK, SelectResponse{
			Purpose: PurposeSubscriptionAdmin,
			Admin:   &AdminDecision{Allowed: false, Reason: "internal_error"},
			Error:   "internal_error",
			Message: "failed to resolve subscription administration",
		})
		return
	}

	h.logger.Debug("Subscription-admin resolved",
		"purpose", PurposeSubscriptionAdmin,
		"requestedSubscription", request.RequestedSubscription,
		"namespace", scope.Namespace,
		"organizationId", scope.OrganizationID,
	)
	c.JSON(http.StatusOK, SelectResponse{
		Purpose: PurposeSubscriptionAdmin,
		Admin: &AdminDecision{
			Allowed: true,
			Scope:   scope,
		},
	})
}

// ListSubscriptions handles GET /v1/subscriptions.
// Returns all subscriptions the authenticated user has access to.
// When no user context is present (ExtractUserInfoOptional did not set one),
// an empty list is returned gracefully.
func (h *Handler) ListSubscriptions(c *gin.Context) {
	h = h.withContext(c)

	c.Header("Cache-Control", "no-store")
	identityVal, identityExists := c.Get("verified_identity")
	var identity *token.VerifiedIdentity
	if identityExists {
		verifiedIdentity, ok := identityVal.(*token.VerifiedIdentity)
		if !ok {
			h.logger.Error("Invalid verified identity context type")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}
		identity = verifiedIdentity
	}

	userContextVal, userExists := c.Get("user")
	if !identityExists && !userExists {
		// No identity headers from Authorino — return an empty list instead
		// of failing. This mirrors the /v1/models behaviour when no
		// LLMInferenceService is deployed.
		h.logger.Debug("No auth context present, returning empty subscription list")
		c.JSON(http.StatusOK, []SubscriptionInfo{})
		return
	}

	var userContext *token.UserContext
	if userExists {
		contextValue, ok := userContextVal.(*token.UserContext)
		if !ok {
			h.logger.Error("Invalid user context type")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"message": "Internal server error",
					"type":    "server_error",
				}})
			return
		}
		userContext = contextValue
	}

	var groups []string
	var username string
	if userContext != nil {
		groups = userContext.Groups
		username = userContext.Username
	}

	accessible, err := h.selector.ListInventory(groups, username, identity)
	if err != nil {
		h.logger.Error("Failed to list subscriptions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": "Failed to list subscriptions",
				"type":    "server_error",
			}})
		return
	}

	c.JSON(http.StatusOK, accessible)
}

// ListSubscriptionsForModel handles GET /v1/model/:model-id/subscriptions.
// Returns subscriptions the user has access to that include the specified model.
// When no user context is present (ExtractUserInfoOptional did not set one),
// an empty list is returned gracefully.
func (h *Handler) ListSubscriptionsForModel(c *gin.Context) {
	h = h.withContext(c)

	c.Header("Cache-Control", "no-store")
	userContextVal, exists := c.Get("user")
	if !exists {
		// No identity headers from Authorino — return an empty list.
		h.logger.Debug("No auth context present, returning empty subscription list for model")
		c.JSON(http.StatusOK, []SubscriptionInfo{})
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

func (h *Handler) recordRejection(reason string) {
	if h.metrics != nil {
		h.metrics.RecordRejection(reason)
	}
}

func (h *Handler) recordUnhealthyRejection(err *ModelUnhealthyError) {
	switch {
	case err.Reason == "RateLimitNotEnforced":
		h.recordRejection(constant.RejectionRateLimited)
	case err.Phase == PhaseFailed:
		h.recordRejection(constant.RejectionQuotaExceeded)
	default:
		h.recordRejection(constant.RejectionNoCapacity)
	}
}
