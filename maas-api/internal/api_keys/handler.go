package api_keys

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// API key creation: single client-visible outcome for subscription resolution failures so we do not
// distinguish not-found, access denied, or no default subscription (enumeration / permission hints).
const (
	apiKeySubscriptionResolutionErrCode = "invalid_subscription"
	apiKeySubscriptionResolutionErrMsg  = "Unable to resolve a subscription for this API key" //nolint:gosec // G101: public JSON error text, not a credential
)

// Regular expressions to match invalid control characters in key name and label values.
var invalidKeyNameCharsPattern = regexp.MustCompile(`[\x00-\x1F\x7F]`)
var invalidLabelCharsPattern = regexp.MustCompile(`[\x00-\x1F\x7F]`)

// Kubernetes-style label keys: optional DNS prefix + name
// https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#syntax-and-character-set
// Prefix: DNS subdomain (alphanumeric, dots, hyphens) ending with /
// Name: alphanumeric, dots, underscores, hyphens
// Examples: "environment", "app.kubernetes.io/name", "company.com/project-id".
var validLabelKeyPattern = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?/)?[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

// AdminChecker is an interface for checking if a user is an admin.
// The SARAdminChecker implementation uses Kubernetes SubjectAccessReview
// to check if the user can create maasauthpolicies (RBAC-based admin detection).
type AdminChecker interface {
	IsAdmin(ctx context.Context, user *token.UserContext) (bool, error)
}

type SubscriptionAdminResolver interface {
	ResolveSubscriptionAdmin(identity *token.VerifiedIdentity, requestedSubscription string) (*subscription.AdminScope, error)
	CanonicalizeSubscription(requestedSubscription string) (string, error)
}

type Handler struct {
	service       *Service
	logger        *logger.Logger
	adminChecker  AdminChecker
	adminResolver SubscriptionAdminResolver
	metrics       MetricsRecorder
}

// MetricsRecorder is the subset of metrics.MetricsRecorder used by this handler.
type MetricsRecorder interface {
	RecordKeyValidation(tenant, result string)
	RecordTokenMint(tenant, result string)
	RecordRejection(reason string)
}

func (h *Handler) withContext(c *gin.Context) *Handler {
	requestHandler := *h
	if requestLogger := middleware.GetLogger(c); requestLogger != nil {
		requestHandler.logger = requestLogger
	} else {
		requestHandler.logger = h.logger.WithContext(c.Request.Context())
	}
	return &requestHandler
}

func (h *Handler) GetAPIKeyConfig(c *gin.Context) {
	h = h.withContext(c)

	if rejectAPIKeyManagement(c) {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"max_expiration_days":      h.service.GetMaxExpirationDays(),
		"ephemeral_max_expiration": constant.DefaultEphemeralKeyMaxExpiration.String(),
	})
}

func NewHandler(
	log *logger.Logger,
	service *Service,
	adminChecker AdminChecker,
	metrics MetricsRecorder,
	adminResolver SubscriptionAdminResolver,
) *Handler {
	if log == nil {
		log = logger.Production()
	}
	if adminChecker == nil {
		panic("adminChecker cannot be nil")
	}
	if adminResolver == nil {
		panic("adminResolver cannot be nil")
	}
	return &Handler{
		service:       service,
		logger:        log,
		adminChecker:  adminChecker,
		adminResolver: adminResolver,
		metrics:       metrics,
	}
}

// getUserContext extracts and validates the user context from the Gin context.
// Returns the user context on success, or responds with an error and returns nil.
func (h *Handler) getUserContext(c *gin.Context) *token.UserContext {
	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return nil
	}

	user, ok := userCtx.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return nil
	}

	return user
}

// reqLogger returns the per-request logger enriched with tenant context,
// falling back to the handler's base logger for internal routes.
func (h *Handler) reqLogger(c *gin.Context) *logger.Logger {
	if l := middleware.GetLogger(c); l != nil {
		return l
	}
	return h.logger
}

// isAdmin checks if the user has admin privileges via SubjectAccessReview.
// Admin is determined by RBAC: can user create maasauthpolicies in the configured MaaS namespace?
// Returns true if the user has admin RBAC permissions, false otherwise.
func (h *Handler) isAdmin(ctx context.Context, user *token.UserContext) (bool, error) {
	if h == nil || user == nil {
		return false, nil
	}
	return h.adminChecker.IsAdmin(ctx, user)
}

func (h *Handler) verifiedIdentity(c *gin.Context) (*token.VerifiedIdentity, bool) {
	value, exists := c.Get("verified_identity")
	if !exists {
		return nil, true
	}
	identity, ok := value.(*token.VerifiedIdentity)
	if !ok || identity == nil {
		return nil, false
	}
	return identity, true
}

func (h *Handler) resolveSubscriptionAdmin(
	identity *token.VerifiedIdentity,
	requestedSubscription string,
	user *token.UserContext,
) (*subscription.AdminScope, *subscription.SubscriptionAdminError, error) {
	if h.adminResolver == nil {
		return nil, nil, errors.New("subscription admin resolver is not configured")
	}
	if identity == nil || requestedSubscription == "" || user == nil {
		return nil, nil, nil
	}
	scope, err := h.adminResolver.ResolveSubscriptionAdmin(identity, requestedSubscription)
	if err != nil {
		var adminErr *subscription.SubscriptionAdminError
		if errors.As(err, &adminErr) {
			return nil, adminErr, nil
		}
		return nil, nil, err
	}
	if scope == nil || scope.Tenant != user.Tenant || scope.Subscription == "" {
		return nil, &subscription.SubscriptionAdminError{
			Code:    "access_denied",
			Message: "organization administration is denied",
		}, nil
	}
	return scope, nil, nil
}

func (h *Handler) canonicalizeSubscription(
	requestedSubscription string,
) (string, *subscription.SubscriptionAdminError, error) {
	if h.adminResolver == nil {
		return "", nil, errors.New("subscription admin resolver is not configured")
	}
	canonicalSubscription, err := h.adminResolver.CanonicalizeSubscription(requestedSubscription)
	if err != nil {
		var adminErr *subscription.SubscriptionAdminError
		if errors.As(err, &adminErr) {
			return "", adminErr, nil
		}
		return "", nil, err
	}
	if canonicalSubscription == "" {
		return "", &subscription.SubscriptionAdminError{
			Code:    "internal_error",
			Message: "subscription canonicalization is invalid",
		}, nil
	}
	return canonicalSubscription, nil, nil
}

type delegatedAdminAuditEvent struct {
	AuthorizationPath  string
	Action             string
	Outcome            string
	Reason             string
	DryRun             bool
	User               *token.UserContext
	Identity           *token.VerifiedIdentity
	Scope              *subscription.AdminScope
	KeyID              string
	TargetUsername     string
	TargetSubscription string
}

func delegatedAdminAuditFields(event delegatedAdminAuditEvent, requestID string) []any {
	tenant := event.User.Tenant
	namespace := ""
	subscription := event.TargetSubscription
	organizationID := ""
	if event.Scope != nil {
		tenant = event.Scope.Tenant
		namespace = event.Scope.Namespace
		subscription = event.Scope.Subscription
		organizationID = event.Scope.OrganizationID
	}

	authorizationPath := event.AuthorizationPath
	if authorizationPath == "" {
		authorizationPath = "delegated-subscription-admin"
	}

	fields := []any{
		"audit.action", event.Action,
		"audit.authorizationPath", authorizationPath,
		"audit.actor", logger.RedactValue(event.User.Username),
		"audit.tenant", tenant,
		"audit.scope", "subscription-admin",
		"audit.outcome", event.Outcome,
	}
	if event.Identity != nil {
		fields = append(fields,
			"audit.actorIssuer", event.Identity.Claims.Issuer,
			"audit.actorSubject", logger.RedactValue(event.Identity.Claims.Subject),
		)
	}
	if namespace != "" {
		fields = append(fields, "audit.namespace", namespace)
	}
	if subscription != "" {
		fields = append(fields, "audit.targetSubscription", logger.RedactValue(subscription))
	}
	if organizationID != "" {
		fields = append(fields, "audit.organizationId", organizationID)
	}
	if event.TargetUsername != "" {
		fields = append(fields, "audit.targetUser", logger.RedactValue(event.TargetUsername))
	}
	if event.KeyID != "" {
		fields = append(fields, "audit.keyId", event.KeyID)
	}
	if event.Reason != "" {
		fields = append(fields, "audit.reason", event.Reason)
	}
	if event.DryRun {
		fields = append(fields, "audit.dryRun", true)
	}
	if requestID != "" {
		fields = append(fields, "audit.requestId", requestID)
	}
	return fields
}

func (h *Handler) emitDelegatedAdminAudit(c *gin.Context, event *delegatedAdminAuditEvent) {
	if event == nil {
		return
	}
	h.reqLogger(c).Info("AUDIT delegated API key administration",
		delegatedAdminAuditFields(*event, middleware.GetRequestID(c))...,
	)
}

func (h *Handler) emitDelegatedAdminAuditResult(
	c *gin.Context,
	event *delegatedAdminAuditEvent,
	outcome string,
	reason string,
) {
	if event == nil {
		return
	}
	if outcome != "" {
		event.Outcome = outcome
	}
	if reason != "" {
		event.Reason = reason
	}
	h.emitDelegatedAdminAudit(c, event)
}

func (h *Handler) isAuthorizedForKey(
	ctx context.Context,
	user *token.UserContext,
	identity *token.VerifiedIdentity,
	key *ApiKey,
	action string,
) (bool, *delegatedAdminAuditEvent, error) {
	if user.Tenant != key.Tenant {
		return false, nil, nil
	}
	if user.Username == key.Username {
		return true, nil, nil
	}
	var delegatedAudit *delegatedAdminAuditEvent
	if identity != nil && key.Subscription != "" {
		scope, _, err := h.resolveSubscriptionAdmin(identity, key.Subscription, user)
		if err != nil {
			return false, &delegatedAdminAuditEvent{
				Action:             action,
				Outcome:            "error",
				Reason:             "resolution_error",
				User:               user,
				Identity:           identity,
				KeyID:              key.ID,
				TargetUsername:     key.Username,
				TargetSubscription: key.Subscription,
			}, err
		}
		if scope != nil && scope.Subscription == key.Subscription {
			return true, &delegatedAdminAuditEvent{
				Action:         action,
				Outcome:        "allowed",
				User:           user,
				Identity:       identity,
				Scope:          scope,
				KeyID:          key.ID,
				TargetUsername: key.Username,
			}, nil
		}
		delegatedAudit = &delegatedAdminAuditEvent{
			Action:             action,
			Outcome:            "denied",
			Reason:             "subscription_scope_denied",
			User:               user,
			Identity:           identity,
			KeyID:              key.ID,
			TargetUsername:     key.Username,
			TargetSubscription: key.Subscription,
		}
	}
	isAdmin, err := h.isAdmin(ctx, user)
	if err != nil {
		if delegatedAudit != nil {
			delegatedAudit.Outcome = "error"
			delegatedAudit.Reason = "admin_status_error"
		}
		return false, delegatedAudit, err
	}
	if delegatedAudit != nil && isAdmin {
		delegatedAudit.Outcome = "allowed"
		delegatedAudit.Reason = "platform_admin_fallback"
		delegatedAudit.AuthorizationPath = "platform-admin-fallback"
	}
	return isAdmin, delegatedAudit, nil
}

func (h *Handler) recordTokenMint(tenant, result string) {
	if h.metrics != nil {
		h.metrics.RecordTokenMint(tenant, result)
	}
}

func (h *Handler) GetAPIKey(c *gin.Context) {
	h = h.withContext(c)

	if rejectAPIKeyManagement(c) {
		return
	}

	tokenID := c.Param("id")
	if tokenID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Token ID required"})
		return
	}

	// Extract user context for authorization
	user := h.getUserContext(c)
	if user == nil {
		return
	}
	identity, validIdentity := h.verifiedIdentity(c)
	if !validIdentity {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid verified identity context type"})
		return
	}

	// Get the API key to check ownership
	tok, err := h.service.GetAPIKey(c.Request.Context(), tokenID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to get API key",
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
		return
	}

	// Check authorization - user must be in same tenant and own the key or be admin
	authorized, delegatedAudit, authErr := h.isAuthorizedForKey(c.Request.Context(), user, identity, tok, "api-key-get")
	if authErr != nil {
		h.emitDelegatedAdminAudit(c, delegatedAudit)
		h.logger.Error("Failed to check admin status", "error", authErr)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}
	if !authorized {
		h.emitDelegatedAdminAudit(c, delegatedAudit)
		h.logger.Warn("Unauthorized API key access attempt",
			"requestingUser", logger.RedactValue(user.Username),
			"keyOwner", logger.RedactValue(tok.Username),
			"keyId", tokenID,
		)
		// Return 404 instead of 403 to prevent key enumeration (IDOR protection)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	h.emitDelegatedAdminAudit(c, delegatedAudit)
	c.JSON(http.StatusOK, tok)
}

// CreateAPIKeyRequest is the request body for creating an API key.
// Name is required for regular keys but optional for ephemeral keys.
// If expiresIn is not provided, defaults to API_KEY_MAX_EXPIRATION_DAYS (or 1hr for ephemeral).
// Users can only create keys for themselves - the key inherits the user's groups.
type CreateAPIKeyRequest struct {
	Name         string            `json:"name,omitempty"` // Required for regular keys, optional for ephemeral
	Description  string            `json:"description,omitempty"`
	Subscription string            `json:"subscription,omitempty"` // Optional MaaSSubscription name; when omitted, highest-priority accessible subscription is used
	ExpiresIn    *token.Duration   `json:"expiresIn,omitempty"`    // Optional - defaults to API_KEY_MAX_EXPIRATION_DAYS (1hr for ephemeral)
	Ephemeral    bool              `json:"ephemeral,omitempty"`    // Short-lived programmatic token (default: false)
	Labels       map[string]string `json:"labels,omitempty"`       // Structured key-value pairs for API key metadata
}

// CreateAPIKey handles POST /v1/api-keys
// Creates a new API key (sk-oai-* format) per Feature Refinement.
// If expiresIn is not provided, defaults to API_KEY_MAX_EXPIRATION_DAYS (1hr for ephemeral).
// Per "Keys Shown Only Once": key is returned ONCE at creation and never again.
// Users can only create keys for themselves - the key inherits the user's groups.
func (h *Handler) CreateAPIKey(c *gin.Context) {
	h = h.withContext(c)

	// API keys are inference credentials, not credentials for minting more API keys.
	// Reject them here, even if the gateway has already authenticated the key, so a
	// subscription-scoped key cannot recover the broader permissions of the user and
	// groups stored in its metadata or extend its own lifetime through a child key.
	if hasAPIKeyCredential(c) {
		h.recordTokenMint(h.service.GetTenantName(), "rejected")
		c.JSON(http.StatusForbidden, gin.H{"error": "API keys cannot create API keys"})
		return
	}

	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}

	// Validate name requirement for non-ephemeral keys
	if !req.Ephemeral && req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required for non-ephemeral keys"})
		return
	}

	// Validate labels if provided
	if err := validateLabels(req.Labels); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Auto-generate name for ephemeral keys if not provided
	name := req.Name
	if req.Ephemeral && name == "" {
		name = fmt.Sprintf("ephemeral-%d", time.Now().UnixNano())
	} else {
		name = strings.TrimSpace(name)
		if len(name) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key name cannot be whitespace only"})
			return
		}
		if utf8.RuneCountInString(name) > 128 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key name cannot exceed 128 characters"})
			return
		}
		if invalidKeyNameCharsPattern.MatchString(name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key name contains invalid control characters"})
			return
		}
	}

	// Parse expiration duration if provided
	var expiresIn *time.Duration
	if req.ExpiresIn != nil {
		d := req.ExpiresIn.Duration
		expiresIn = &d
	} else if req.Ephemeral {
		// Default 1hr expiration for ephemeral keys
		d := 1 * time.Hour
		expiresIn = &d
	}

	// Create key for the authenticated user with their groups and tenant
	result, err := h.service.CreateAPIKey(
		c.Request.Context(),
		user.Username,
		user.Groups,
		name,
		req.Description,
		expiresIn,
		req.Ephemeral,
		strings.TrimSpace(req.Subscription),
		user.Tenant,
		req.Labels,
	)
	if err != nil {
		h.logger.Error("Failed to create API key", "error", err)
		if errors.Is(err, ErrExpirationNotPositive) || errors.Is(err, ErrExpirationExceedsMax) || errors.Is(err, ErrInvalidGroupName) {
			h.recordTokenMint(user.Tenant, "rejected")
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var notFound *subscription.SubscriptionNotFoundError
		var accessDenied *subscription.AccessDeniedError
		var noSub *subscription.NoSubscriptionError
		var multipleSubs *subscription.MultipleSubscriptionsError
		var modelNotInSub *subscription.ModelNotInSubscriptionError
		var modelUnhealthy *subscription.ModelUnhealthyError
		if errors.As(err, &notFound) || errors.As(err, &accessDenied) || errors.As(err, &noSub) ||
			errors.As(err, &multipleSubs) || errors.As(err, &modelNotInSub) {
			h.recordTokenMint(user.Tenant, "rejected")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": apiKeySubscriptionResolutionErrMsg,
				"code":  apiKeySubscriptionResolutionErrCode,
			})
			return
		}
		if errors.As(err, &modelUnhealthy) {
			h.recordTokenMint(user.Tenant, "rejected")
			statusCode := http.StatusBadRequest
			if modelUnhealthy.Phase == "Failed" {
				statusCode = http.StatusForbidden
			}
			c.JSON(statusCode, gin.H{
				"error": modelUnhealthy.Message,
				"code":  "subscription_not_ready",
			})
			return
		}
		h.recordTokenMint(user.Tenant, "failure")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create API key"})
		return
	}

	h.recordTokenMint(user.Tenant, "success")

	h.reqLogger(c).Info("Created API key",
		"keyId", result.ID,
		"keyPrefix", result.KeyPrefix,
		"username", logger.RedactValue(user.Username),
		"ephemeral", req.Ephemeral,
	)

	// Return the key - THIS IS THE ONLY TIME THE PLAINTEXT IS SHOWN
	c.JSON(http.StatusCreated, result)
}

func isAPIKeyBearer(authorization string) bool {
	parts := strings.Fields(authorization)
	return len(parts) > 1 && strings.EqualFold(parts[0], "Bearer") &&
		strings.HasPrefix(strings.Join(parts[1:], " "), KeyPrefix)
}

func hasAPIKeyCredential(c *gin.Context) bool {
	return isAPIKeyBearer(c.GetHeader("Authorization")) ||
		strings.HasPrefix(strings.TrimSpace(c.GetHeader("X-API-Key")), KeyPrefix)
}

func rejectAPIKeyManagement(c *gin.Context) bool {
	if !hasAPIKeyCredential(c) {
		return false
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "API keys cannot manage API keys"})
	return true
}

// validateLabels validates the labels map for security and size constraints.
// Labels are user-defined key-value pairs for organizing and filtering API keys.
func validateLabels(labels map[string]string) error {
	if labels == nil {
		return nil
	}

	// Limit number of label entries (prevent abuse)
	if len(labels) > constant.MaxLabelsEntries {
		return fmt.Errorf("labels cannot exceed %d key-value pairs", constant.MaxLabelsEntries)
	}

	// Validate each key-value pair
	for key, value := range labels {
		// Key validation
		if len(key) == 0 {
			return errors.New("label keys cannot be empty")
		}
		if len(key) > constant.MaxLabelKeyLength {
			return fmt.Errorf("label key '%s' exceeds %d characters", key, constant.MaxLabelKeyLength)
		}
		// Only allow alphanumerics, underscores, hyphens, dots (similar to K8s labels).
		// Use a package-level variable for comparison to avoid recomputing the regex every iteration of the loop.
		if !validLabelKeyPattern.MatchString(key) {
			return fmt.Errorf("label key '%s' contains invalid characters (only alphanumerics, dots, underscores, hyphens allowed)", key)
		}

		// Value validation
		if len(value) == 0 {
			return fmt.Errorf("label value for key '%s' cannot be empty", key)
		}
		if len(value) > constant.MaxLabelValueLength {
			return fmt.Errorf("label value for key '%s' exceeds %d characters", key, constant.MaxLabelValueLength)
		}

		// Reject control characters in values
		if invalidLabelCharsPattern.MatchString(value) {
			return fmt.Errorf("label value for key '%s' contains invalid control characters", key)
		}
	}

	return nil
}

// ValidateAPIKeyRequest is the request body for validating an API key.
type ValidateAPIKeyRequest struct {
	Key string `binding:"required" json:"key"`
}

// ValidateAPIKeyHandler handles POST /internal/v1/api-keys/validate
// This endpoint is called by Authorino via HTTP external auth callback
// Per Feature Refinement "Gateway Integration (Inference Flow)".
func (h *Handler) ValidateAPIKeyHandler(c *gin.Context) {
	h = h.withContext(c)

	var req ValidateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
		return
	}

	result, err := h.service.ValidateAPIKey(c.Request.Context(), req.Key)
	if err != nil {
		if h.metrics != nil {
			h.metrics.RecordKeyValidation(h.service.GetTenantName(), "error")
		}
		h.logger.Error("API key validation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "validation failed"})
		return
	}

	if h.metrics != nil {
		validResult := "invalid"
		if result.Valid {
			validResult = "valid"
		}
		h.metrics.RecordKeyValidation(result.Tenant, validResult)
	}

	if !result.Valid {
		// Record rejection for invalid API key
		if h.metrics != nil {
			h.metrics.RecordRejection(constant.RejectionUnauthorized)
		}
		// Return 200 with validation result for Authorino
		// Per design doc section 7.7: invalid keys should return 200 with valid:false
		c.JSON(http.StatusOK, result)
		return
	}

	// Valid key - return user identity for Authorino to use
	c.JSON(http.StatusOK, result)
}

// RevokeAPIKey handles DELETE /v1/api-keys/:id
// Revokes a specific API key by changing its status to 'revoked'.
func (h *Handler) RevokeAPIKey(c *gin.Context) {
	h = h.withContext(c)

	if rejectAPIKeyManagement(c) {
		return
	}

	keyID := c.Param("id")
	if keyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "API key ID required"})
		return
	}

	// Extract user context for authorization
	user := h.getUserContext(c)
	if user == nil {
		return
	}
	identity, validIdentity := h.verifiedIdentity(c)
	if !validIdentity {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid verified identity context type"})
		return
	}

	// Get the API key to check ownership before revoking
	keyMetadata, err := h.service.GetAPIKey(c.Request.Context(), keyID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to get API key for authorization check", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve API key"})
		return
	}

	// Check authorization - user must be in same tenant and own the key or be admin
	authorized, delegatedAudit, authErr := h.isAuthorizedForKey(c.Request.Context(), user, identity, keyMetadata, "api-key-revoke")
	if authErr != nil {
		h.emitDelegatedAdminAudit(c, delegatedAudit)
		h.logger.Error("Failed to check admin status", "error", authErr)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}
	if !authorized {
		h.emitDelegatedAdminAudit(c, delegatedAudit)
		h.logger.Warn("Unauthorized API key revocation attempt",
			"requestingUser", logger.RedactValue(user.Username),
			"keyOwner", logger.RedactValue(keyMetadata.Username),
			"keyId", keyID,
		)
		// Return 404 instead of 403 to prevent key enumeration (IDOR protection)
		c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
		return
	}

	// Perform the revocation
	if err := h.service.RevokeAPIKey(c.Request.Context(), keyID); err != nil {
		h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "request_failed")
		if errors.Is(err, ErrKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "API key not found"})
			return
		}
		h.logger.Error("Failed to revoke API key", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke API key"})
		return
	}

	h.reqLogger(c).Info("Revoked API key", "keyId", keyID, "revokedBy", logger.RedactValue(user.Username))

	// Return the revoked key metadata (per OpenAPI spec)
	if delegatedAudit != nil {
		delegatedAudit.Outcome = "allowed"
	}
	revokedKey, err := h.service.GetAPIKey(c.Request.Context(), keyID)
	if err != nil {
		h.emitDelegatedAdminAuditResult(c, delegatedAudit, "allowed", "metadata_retrieval_failed")
		h.logger.Error("Failed to retrieve revoked key", "error", err, "keyId", keyID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Key revoked but failed to retrieve metadata"})
		return
	}

	h.emitDelegatedAdminAudit(c, delegatedAudit)
	c.JSON(http.StatusOK, revokedKey)
}

func applySearchAPIKeysDefaults(req *SearchAPIKeysRequest) {
	if req.Filters == nil {
		req.Filters = &SearchFilters{}
	}
	if req.Sort == nil {
		req.Sort = &SortParams{
			By:    DefaultSortBy,
			Order: DefaultSortOrder,
		}
	}
	if req.Pagination == nil {
		req.Pagination = &PaginationParams{
			Limit:  DefaultLimit,
			Offset: 0,
		}
	}
}

func validateSearchAPIKeysRequest(c *gin.Context, req *SearchAPIKeysRequest) bool {
	for _, status := range req.Filters.Status {
		trimmed := strings.TrimSpace(status)
		if !ValidStatuses[trimmed] {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("invalid status '%s': must be active, revoked, or expired", status),
			})
			return false
		}
	}

	if req.Sort.By != "" && !ValidSortFields[req.Sort.By] {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid sort.by: must be one of: created_at, expires_at, last_used_at, name",
		})
		return false
	}

	if req.Sort.Order != "" {
		orderLower := strings.ToLower(strings.TrimSpace(req.Sort.Order))
		if !ValidSortOrders[orderLower] {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid sort.order: must be asc or desc",
			})
			return false
		}
		req.Sort.Order = orderLower
	}

	if req.Pagination.Limit < 1 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "pagination.limit must be at least 1",
		})
		return false
	}
	if req.Pagination.Limit > MaxLimit {
		req.Pagination.Limit = MaxLimit
	}
	if req.Pagination.Offset < 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "pagination.offset must be non-negative",
		})
		return false
	}

	if err := validateLabels(req.Filters.LabelsContain); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	return true
}

// SearchAPIKeys handles POST /v1/api-keys/search
// Searches API keys with flexible filtering, sorting, and pagination.
// When no user context is present (ExtractUserInfoOptional did not set one),
// an empty list is returned gracefully.
func (h *Handler) SearchAPIKeys(c *gin.Context) {
	h = h.withContext(c)

	if rejectAPIKeyManagement(c) {
		return
	}

	c.Header("Cache-Control", "no-store")
	userContextVal, exists := c.Get("user")
	if !exists {
		// No identity headers from Authorino — return an empty list instead
		// of failing.  This mirrors the /v1/models behaviour when no
		// LLMInferenceService is deployed.
		h.logger.Debug("No auth context present, returning empty API key list")
		c.JSON(http.StatusOK, SearchAPIKeysResponse{
			Object: "list",
			Data:   []ApiKey{},
		})
		return
	}

	var req SearchAPIKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, ok := userContextVal.(*token.UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return
	}
	identity, validIdentity := h.verifiedIdentity(c)
	if !validIdentity {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid verified identity context type"})
		return
	}

	applySearchAPIKeysDefaults(&req)
	if !validateSearchAPIKeysRequest(c, &req) {
		return
	}

	targetUsername := strings.TrimSpace(req.Filters.Username)
	requestedSubscription := ""
	if req.Filters.Subscription != nil {
		requestedSubscription = strings.TrimSpace(*req.Filters.Subscription)
	}

	delegatedAdmin := false
	var delegatedAudit *delegatedAdminAuditEvent
	if identity != nil && requestedSubscription != "" {
		scope, _, resolveErr := h.resolveSubscriptionAdmin(identity, requestedSubscription, user)
		if resolveErr != nil {
			h.logger.Error("Failed to resolve subscription administration", "error", resolveErr)
			h.emitDelegatedAdminAudit(c, &delegatedAdminAuditEvent{
				Action:             "api-key-search",
				Outcome:            "error",
				Reason:             "resolution_error",
				User:               user,
				Identity:           identity,
				TargetUsername:     targetUsername,
				TargetSubscription: requestedSubscription,
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
			return
		}
		if scope != nil {
			delegatedAdmin = true
			canonicalSubscription := scope.Subscription
			req.Filters.Subscription = &canonicalSubscription
			delegatedAudit = &delegatedAdminAuditEvent{
				Action:         "api-key-search",
				Outcome:        "allowed",
				User:           user,
				Identity:       identity,
				Scope:          scope,
				TargetUsername: targetUsername,
			}
		} else {
			delegatedAudit = &delegatedAdminAuditEvent{
				Action:             "api-key-search",
				Outcome:            "denied",
				Reason:             "subscription_admin_denied",
				User:               user,
				Identity:           identity,
				TargetUsername:     targetUsername,
				TargetSubscription: requestedSubscription,
			}
		}
	}

	isAdmin := false
	if !delegatedAdmin {
		var adminErr error
		isAdmin, adminErr = h.isAdmin(c.Request.Context(), user)
		if adminErr != nil {
			h.logger.Error("Failed to check admin status", "error", adminErr)
			h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "admin_status_error")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
			return
		}
	}

	if !delegatedAdmin && !isAdmin {
		if targetUsername != "" && targetUsername != user.Username {
			h.emitDelegatedAdminAudit(c, delegatedAudit)
			c.JSON(http.StatusForbidden, gin.H{
				"error": "non-admin users can only search their own API keys",
			})
			return
		}
		targetUsername = user.Username
	}

	if !delegatedAdmin && requestedSubscription != "" {
		canonicalSubscription, adminErr, err := h.canonicalizeSubscription(requestedSubscription)
		if err != nil {
			h.logger.Error("Failed to canonicalize subscription", "error", err)
			h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "subscription_canonicalization_error")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
			return
		}
		if adminErr != nil {
			if !isAdmin {
				h.emitDelegatedAdminAuditResult(c, delegatedAudit, "denied", "subscription_canonicalization_failed")
				c.JSON(http.StatusOK, SearchAPIKeysResponse{
					Object: "list",
					Data:   []ApiKey{},
				})
				return
			}
			h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "subscription_canonicalization_failed")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": apiKeySubscriptionResolutionErrMsg,
				"code":  apiKeySubscriptionResolutionErrCode,
			})
			return
		}
		req.Filters.Subscription = &canonicalSubscription
		if delegatedAudit != nil {
			delegatedAudit.TargetSubscription = canonicalSubscription
			delegatedAudit.Outcome = "allowed"
			if isAdmin {
				delegatedAudit.Reason = "platform_admin_fallback"
				delegatedAudit.AuthorizationPath = "platform-admin-fallback"
			} else {
				delegatedAudit.Reason = "owner_fallback"
				delegatedAudit.AuthorizationPath = "owner-fallback"
			}
		}
	}

	// Call service layer (tenant scoping is mandatory)
	result, err := h.service.Search(
		c.Request.Context(),
		targetUsername,
		user.Tenant,
		req.Filters,
		req.Sort,
		req.Pagination,
	)
	if err != nil {
		h.logger.Error("Failed to search API keys",
			"error", err,
			"username", logger.RedactValue(targetUsername),
		)
		h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "request_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search API keys"})
		return
	}

	// Build response
	response := SearchAPIKeysResponse{
		Object:  "list",
		Data:    result.Keys,
		HasMore: result.HasMore,
	}

	h.emitDelegatedAdminAudit(c, delegatedAudit)
	c.JSON(http.StatusOK, response)
}

// CleanupExpiredEphemeralKeys handles POST /internal/v1/api-keys/cleanup
// Deletes expired ephemeral API keys. Called by CronJob.
// Access is restricted at the network level via NetworkPolicy.
func (h *Handler) CleanupExpiredEphemeralKeys(c *gin.Context) {
	h = h.withContext(c)

	count, err := h.service.CleanupExpiredEphemeral(c.Request.Context())
	if err != nil {
		h.logger.Error("Failed to cleanup expired ephemeral keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to cleanup expired ephemeral keys"})
		return
	}
	retainedCount, err := h.service.CleanupSoftDeleted(c.Request.Context())
	if err != nil {
		h.logger.Error("Failed to cleanup retained API keys", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to cleanup retained API keys"})
		return
	}

	message := fmt.Sprintf("Successfully deleted %d expired ephemeral key(s)", count)
	if retainedCount > 0 {
		message = fmt.Sprintf("Successfully deleted %d expired ephemeral and %d retained API key(s)", count, retainedCount)
	}
	c.JSON(http.StatusOK, CleanupResponse{
		DeletedCount: count + retainedCount,
		Message:      message,
	})
}

// RevokeTenantAPIKeys handles DELETE /internal/v1/tenants/:tenant/api-keys.
// Revokes all active API keys for this maas-api instance's tenant.
func (h *Handler) RevokeTenantAPIKeys(c *gin.Context) {
	h = h.withContext(c)

	tenant := strings.TrimSpace(c.Param("tenant"))
	count, err := h.service.RevokeTenantAPIKeys(c.Request.Context(), tenant)
	if err != nil {
		h.logger.Error("Failed to revoke tenant API keys", "error", err, "tenant", tenant)
		switch {
		case errors.Is(err, ErrTenantRequired):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrTenantRequired.Error()})
		case errors.Is(err, ErrTenantMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrTenantMismatch.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke tenant API keys"})
		}
		return
	}

	deletedAt := time.Now().UTC()
	h.emitLifecycleCleanupAudit(c, "tenant", tenant, "", count, deletedAt, "aitenant-controller")
	h.logger.Info("Invalidated tenant API keys", "tenant", tenant, "count", count)
	c.JSON(http.StatusOK, TenantRevokeResponse{
		RevokedCount: count,
		DeletedCount: count,
		DeletedAt:    deletedAt,
		Message:      fmt.Sprintf("Successfully invalidated %d API key(s) for tenant %s", count, tenant),
	})
}

// RevokeSubscriptionAPIKeys handles DELETE /internal/v1/tenants/:tenant/subscriptions/:subscription/api-keys.
// It soft-deletes only keys bound to the requested subscription within the tenant.
func (h *Handler) RevokeSubscriptionAPIKeys(c *gin.Context) {
	h = h.withContext(c)

	tenant := strings.TrimSpace(c.Param("tenant"))
	subscription := strings.TrimSpace(c.Param("subscription"))
	count, err := h.service.RevokeSubscriptionAPIKeys(c.Request.Context(), tenant, subscription)
	if err != nil {
		h.logger.Error("Failed to revoke subscription API keys", "error", err,
			"tenant", tenant, "subscription", logger.RedactValue(subscription))
		switch {
		case errors.Is(err, ErrTenantRequired), errors.Is(err, ErrTenantMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke subscription API keys"})
		}
		return
	}

	deletedAt := time.Now().UTC()
	h.emitLifecycleCleanupAudit(c, "subscription", tenant, subscription, count, deletedAt, "maassubscription-controller")
	c.JSON(http.StatusOK, SubscriptionRevokeResponse{
		RevokedCount: count,
		DeletedCount: count,
		DeletedAt:    deletedAt,
		Message:      fmt.Sprintf("Successfully invalidated %d API key(s) for subscription %s", count, subscription),
	})
}

func (h *Handler) emitLifecycleCleanupAudit(c *gin.Context, scope, tenant, subscription string, count int, deletedAt time.Time, initiator string) {
	h.reqLogger(c).Info("AUDIT API-key lifecycle cleanup",
		"audit.action", "api-key-soft-delete",
		"audit.scope", scope,
		"audit.tenant", tenant,
		"audit.subscription", logger.RedactValue(subscription),
		"audit.deletedCount", count,
		"audit.deletedAt", deletedAt.UTC().Format(time.RFC3339),
		"audit.initiator", initiator,
	)
}

// BulkRevokeAPIKeys handles POST /v1/api-keys/bulk-revoke
// Revokes all active API keys matching the scope: by username, subscription, or both.
// Supports dryRun=true to preview how many keys would be revoked without mutating.
// Subscription-scoped revocation is admin-only.
func (h *Handler) BulkRevokeAPIKeys(c *gin.Context) {
	h = h.withContext(c)

	if rejectAPIKeyManagement(c) {
		return
	}

	var req BulkRevokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.Subscription = strings.TrimSpace(req.Subscription)

	if req.Username == "" && req.Subscription == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "at least one of username or subscription is required",
		})
		return
	}

	user := h.getUserContext(c)
	if user == nil {
		return
	}
	identity, validIdentity := h.verifiedIdentity(c)
	if !validIdentity {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid verified identity context type"})
		return
	}

	// Authorization rules:
	// - Subscription-scoped revocation always requires admin
	// - Username-scoped: users can revoke own keys, admins can revoke any user's keys
	needsAdmin := req.Subscription != "" || (req.Username != "" && req.Username != user.Username)
	delegatedAdmin := false
	var delegatedAudit *delegatedAdminAuditEvent
	if needsAdmin && identity != nil && req.Subscription != "" {
		scope, _, resolveErr := h.resolveSubscriptionAdmin(identity, req.Subscription, user)
		if resolveErr != nil {
			h.logger.Error("Failed to resolve subscription administration", "error", resolveErr)
			h.emitDelegatedAdminAudit(c, &delegatedAdminAuditEvent{
				Action:             "api-key-bulk-revoke",
				Outcome:            "error",
				Reason:             "resolution_error",
				User:               user,
				Identity:           identity,
				TargetUsername:     req.Username,
				TargetSubscription: req.Subscription,
			})
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
			return
		}
		if scope != nil {
			req.Subscription = scope.Subscription
			delegatedAdmin = true
			delegatedAudit = &delegatedAdminAuditEvent{
				Action:             "api-key-bulk-revoke",
				Outcome:            "allowed",
				User:               user,
				Identity:           identity,
				Scope:              scope,
				TargetUsername:     req.Username,
				TargetSubscription: req.Subscription,
			}
		} else {
			delegatedAudit = &delegatedAdminAuditEvent{
				Action:             "api-key-bulk-revoke",
				Outcome:            "denied",
				Reason:             "subscription_admin_denied",
				User:               user,
				Identity:           identity,
				TargetUsername:     req.Username,
				TargetSubscription: req.Subscription,
			}
		}
	}
	if needsAdmin && !delegatedAdmin {
		isAdmin, adminErr := h.isAdmin(c.Request.Context(), user)
		if adminErr != nil {
			h.logger.Error("Failed to check admin status", "error", adminErr)
			h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "admin_status_error")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
			return
		}
		if !isAdmin {
			h.emitDelegatedAdminAudit(c, delegatedAudit)
			h.logger.Warn("Unauthorized bulk revoke attempt",
				"requestingUser", logger.RedactValue(user.Username),
				"targetUser", logger.RedactValue(req.Username),
				"targetSubscription", logger.RedactValue(req.Subscription),
			)
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Access denied: admin privileges required for this bulk revoke operation",
			})
			return
		}
		if req.Subscription != "" {
			canonicalSubscription, adminErr, err := h.canonicalizeSubscription(req.Subscription)
			if err != nil {
				h.logger.Error("Failed to canonicalize subscription", "error", err)
				h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "subscription_canonicalization_error")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check authorization"})
				return
			}
			if adminErr != nil {
				h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "subscription_canonicalization_failed")
				c.JSON(http.StatusBadRequest, gin.H{"error": adminErr.Message})
				return
			}
			req.Subscription = canonicalSubscription
			if delegatedAudit != nil {
				delegatedAudit.TargetSubscription = canonicalSubscription
				delegatedAudit.Outcome = "allowed"
				delegatedAudit.Reason = "platform_admin_fallback"
				delegatedAudit.AuthorizationPath = "platform-admin-fallback"
			}
		}
	}

	scopeDesc := h.bulkRevokeScopeDescription(req)
	logScope := h.bulkRevokeLogScope(req)

	// Dry-run: return count of keys that would be revoked without mutating
	if req.DryRun {
		count, err := h.service.BulkRevokeAPIKeys(c.Request.Context(), req.Username, req.Subscription, user.Tenant, true)
		if err != nil {
			h.logger.Error("Dry-run bulk revoke failed", "error", err)
			h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "request_failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to perform dry-run"})
			return
		}

		h.reqLogger(c).Info("Dry-run bulk revoke",
			"count", count,
			"scope", logScope,
			"actor", logger.RedactValue(user.Username),
		)

		if delegatedAudit != nil {
			delegatedAudit.DryRun = true
		}
		h.emitDelegatedAdminAuditResult(c, delegatedAudit, "allowed", "")
		c.JSON(http.StatusOK, BulkRevokeResponse{
			RevokedCount: count,
			Message:      fmt.Sprintf("Dry run: %d active key(s) would be revoked for %s", count, scopeDesc),
			DryRun:       true,
		})
		return
	}

	// Perform bulk revocation (scoped to caller's tenant)
	count, err := h.service.BulkRevokeAPIKeys(c.Request.Context(), req.Username, req.Subscription, user.Tenant, false)
	if err != nil {
		h.logger.Error("Failed to bulk revoke API keys",
			"error", err,
			"scope", logScope,
			"requestingUser", logger.RedactValue(user.Username),
		)
		h.emitDelegatedAdminAuditResult(c, delegatedAudit, "error", "request_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke API keys"})
		return
	}

	h.reqLogger(c).Info("Bulk revoked API keys",
		"count", count,
		"scope", logScope,
		"revokedBy", logger.RedactValue(user.Username),
	)

	h.emitBulkRevokeAudit(c, user, req, count)

	h.emitDelegatedAdminAudit(c, delegatedAudit)
	c.JSON(http.StatusOK, BulkRevokeResponse{
		RevokedCount: count,
		Message:      fmt.Sprintf("Successfully revoked %d active API key(s) for %s", count, scopeDesc),
	})
}

// bulkRevokeScopeDescription returns a human-readable description for HTTP responses.
func (h *Handler) bulkRevokeScopeDescription(req BulkRevokeRequest) string {
	switch {
	case req.Username != "" && req.Subscription != "":
		return fmt.Sprintf("user %s in subscription %s", req.Username, req.Subscription)
	case req.Subscription != "":
		return fmt.Sprintf("subscription %s", req.Subscription)
	default:
		return fmt.Sprintf("user %s", req.Username)
	}
}

// bulkRevokeLogScope returns a redacted scope description safe for logging (CWE-532).
func (h *Handler) bulkRevokeLogScope(req BulkRevokeRequest) string {
	switch {
	case req.Username != "" && req.Subscription != "":
		return fmt.Sprintf("user %s in subscription %s", logger.RedactValue(req.Username), logger.RedactValue(req.Subscription))
	case req.Subscription != "":
		return fmt.Sprintf("subscription %s", logger.RedactValue(req.Subscription))
	default:
		return fmt.Sprintf("user %s", logger.RedactValue(req.Username))
	}
}

// emitBulkRevokeAudit writes a structured audit record for actual bulk revoke
// operations. Not called for dry-runs — dry-runs are read-only previews and
// should not produce audit records.
// Fields follow a stable schema so log aggregators (Splunk, ELK, CloudWatch)
// can index them without custom parsing.
func (h *Handler) emitBulkRevokeAudit(c *gin.Context, user *token.UserContext, req BulkRevokeRequest, count int) {
	h.reqLogger(c).Info("AUDIT bulk-revoke",
		"audit.action", "bulk-revoke",
		"audit.actor", logger.RedactValue(user.Username),
		"audit.tenant", user.Tenant,
		"audit.targetUser", logger.RedactValue(req.Username),
		"audit.targetSubscription", logger.RedactValue(req.Subscription),
		"audit.revokedCount", count,
	)
}
