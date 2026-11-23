package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/groupname"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type Handler struct {
	tenantName string
	logger     *logger.Logger
	// kubeClient backs the Kubernetes TokenReview fallback. It is nil unless
	// WithKubeClient was called; the fallback then fails closed with a 500.
	kubeClient kubernetes.Interface
}

func NewHandler(log *logger.Logger, tenantName string) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		tenantName: tenantName,
		logger:     log,
	}
}

// WithKubeClient enables the Kubernetes TokenReview fallback in
// ExtractUserInfo/ExtractUserInfoOptional for routes that accept
// OpenShift/Kubernetes bearer tokens (currently the Loki usage-logs proxy).
func (h *Handler) WithKubeClient(kubeClient kubernetes.Interface) *Handler {
	h.kubeClient = kubeClient
	return h
}

// AuthMode selects which authentication mechanisms a route group accepts.
// It declares what a route admits; AuthSource on the resulting UserContext
// records which mechanism actually authenticated the request.
type AuthMode int

const (
	// AuthModeGateway accepts the OIDC identity envelope or trusted
	// gateway-injected identity headers. A bearer token is never validated;
	// missing identity headers are an auth-policy configuration error (500 on
	// strict routes, anonymous pass-through on optional routes). This is the
	// historical behavior of every non-Loki route.
	AuthModeGateway AuthMode = iota

	// AuthModeGatewayOrKubernetes additionally accepts OpenShift/Kubernetes
	// bearer tokens via the TokenReview fallback (requires
	// system:authenticated). Inference credentials (sk-oai- bearer /
	// X-Api-Key) are rejected with 401 rather than falling through. Used by
	// the Loki usage-logs proxy.
	AuthModeGatewayOrKubernetes
)

// ParseGroupsHeader parses the group header which may arrive as a JSON array
// (e.g., ["ai-eng"]) or as an Authorino bracket-wrapped format (e.g., [ai-eng]).
func ParseGroupsHeader(header string) ([]string, error) {
	if header == "" {
		return nil, errors.New("header is empty")
	}
	trimmed := strings.TrimSpace(header)
	if trimmed == "[]" {
		return []string{}, nil
	}

	var parsedGroups []string

	// Try JSON array first (e.g., ["ai-eng", "platform"])
	if err := json.Unmarshal([]byte(header), &parsedGroups); err != nil {
		// Fall back to bracket-wrapped space-separated format (e.g., [ai-eng platform]).
		// Authorino's plain selector serializes arrays this way.
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inner := trimmed[1 : len(trimmed)-1]
			parsedGroups = strings.Fields(inner)
		} else {
			return nil, fmt.Errorf("unsupported group header format: %s", header)
		}
	}

	var validGroups []string
	for _, g := range parsedGroups {
		if g != "" {
			validGroups = append(validGroups, g)
		}
	}

	if len(validGroups) == 0 {
		return nil, errors.New("no groups found in header")
	}

	return validGroups, nil
}

// ExtractUserInfo selects exactly one authentication mechanism per request, by
// request shape:
//
//  1. X-MaaS-Identity envelope present → OIDC mode (AuthSource "oidc").
//  2. Gateway-injected X-MaaS-Username/X-MaaS-Group present → trusted gateway
//     identity (AuthSource "gateway"). Headers win over any bearer token: the
//     gateway forwards Authorization and injects headers for the same request.
//  3. Otherwise, when mode is AuthModeGatewayOrKubernetes, a non-inference
//     bearer token is validated via TokenReview (AuthSource "k8s"). Inference
//     credentials (sk-oai- bearer / X-Api-Key) never authenticate these routes.
//  4. When mode is AuthModeGateway the historical behavior is preserved:
//     missing identity headers are an auth-policy configuration error (500).
//
// Tech debt: only the Loki usage-logs proxy uses AuthModeGatewayOrKubernetes
// today; the remaining routes should eventually be moved onto it so every
// route accepts Kubernetes auth.
func (h *Handler) ExtractUserInfo(mode AuthMode) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestLogger := h.logger.WithContext(c.Request.Context())
		if value, exists := c.Get("verified_identity"); exists {
			if !h.setUserContextFromVerifiedIdentity(c, value) {
				return
			}
			c.Next()
			return
		}

		username := strings.TrimSpace(c.GetHeader(constant.HeaderUsername))
		groupHeader := c.GetHeader(constant.HeaderGroup)

		// Mechanism 2: trusted gateway identity headers. Any presence selects
		// this mechanism; incomplete or malformed headers fail closed below.
		if username != "" || groupHeader != "" {
			h.setUserContextFromGatewayHeaders(c, requestLogger, username, groupHeader)
			return
		}

		if mode == AuthModeGatewayOrKubernetes {
			h.authenticateBearerToken(c, requestLogger, true)
			return
		}

		// Mechanism disabled: missing headers indicate a configuration issue
		// with the auth policy (internal error), preserving historical behavior.
		requestLogger.Error("Missing or empty username header",
			"header", constant.HeaderUsername,
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         "Exception thrown while generating token",
			"exceptionCode": "AUTH_FAILURE",
			"refId":         "001",
		})
		c.Abort()
	}
}

// ExtractUserInfoOptional is a lenient variant of ExtractUserInfo for endpoints
// that must remain accessible when no auth policy is active (e.g. no
// LLMInferenceService deployed). When no identity is present at all the
// middleware continues without setting a user context, letting the handler
// decide what to return. A verified identity takes precedence over legacy
// headers. Otherwise, if headers ARE present but malformed, it still aborts so
// that real configuration errors are surfaced. When mode is
// AuthModeGatewayOrKubernetes, a bearer token without gateway headers
// authenticates via TokenReview (mechanism selection identical to
// ExtractUserInfo).
func (h *Handler) ExtractUserInfoOptional(mode AuthMode) gin.HandlerFunc {
	return func(c *gin.Context) {
		if value, exists := c.Get("verified_identity"); exists {
			if !h.setUserContextFromVerifiedIdentity(c, value) {
				return
			}
			c.Next()
			return
		}

		requestLogger := h.logger.WithContext(c.Request.Context())

		username := strings.TrimSpace(c.GetHeader(constant.HeaderUsername))
		groupHeader := c.GetHeader(constant.HeaderGroup)

		if username != "" && groupHeader != "" {
			h.setUserContextFromGatewayHeaders(c, requestLogger, username, groupHeader)
			return
		}

		if mode == AuthModeGatewayOrKubernetes {
			h.authenticateBearerToken(c, requestLogger, false)
			return
		}

		// Incomplete legacy identity headers are treated as absent. This keeps
		// a verified identity, when present, authoritative and lets handlers
		// reject requests that have no usable identity.
		requestLogger.Debug("Auth identity headers not present or incomplete, continuing without user context")
		c.Next()
	}
}

func (h *Handler) setUserContextFromVerifiedIdentity(c *gin.Context, value any) bool {
	identity, ok := value.(*VerifiedIdentity)
	if !ok || identity == nil {
		requestLogger := h.logger.WithContext(c.Request.Context())
		requestLogger.Error("Invalid verified identity context type")
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         "Exception thrown while generating token",
			"exceptionCode": "AUTH_FAILURE",
			"refId":         "004",
		})
		c.Abort()
		return false
	}

	groups, err := QualifiedGroups(identity)
	if err != nil {
		h.rejectIdentity(c, err)
		return false
	}
	if !groupname.AllValid(identity.Claims.Groups) {
		groups = nil
	}
	groups = append([]string{"system:authenticated"}, groups...)
	username := identity.Claims.PreferredUsername
	if username == "" {
		username = identity.Claims.Subject
	}
	c.Set("user", &UserContext{
		Username:       username,
		Groups:         groups,
		Tenant:         h.tenantName,
		OrganizationID: identity.Claims.OrgID,
		Roles:          append([]string(nil), identity.Claims.Roles...),
		AuthSource:     AuthSourceOIDC,
	})
	return true
}

// setUserContextFromGatewayHeaders validates the gateway-injected identity
// headers and installs the resulting user context. It always terminates the
// middleware chain (successfully or with an error response), so callers must
// only invoke it once the mechanism has been selected.
func (h *Handler) setUserContextFromGatewayHeaders(c *gin.Context, requestLogger *logger.Logger, username, groupHeader string) {
	// Validate required headers exist and are not empty.
	// Missing headers indicate a configuration issue with the auth policy (internal error).
	if username == "" {
		requestLogger.Error("Missing or empty username header",
			"header", constant.HeaderUsername,
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         "Exception thrown while generating token",
			"exceptionCode": "AUTH_FAILURE",
			"refId":         "001",
		})
		c.Abort()
		return
	}

	if groupHeader == "" {
		requestLogger.Error("Missing group header",
			"header", constant.HeaderGroup,
			"username", logger.RedactValue(username),
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         "Exception thrown while generating token",
			"exceptionCode": "AUTH_FAILURE",
			"refId":         "002",
		})
		c.Abort()
		return
	}

	// Parse groups from header - format: "[group1 group2 group3]"
	// Parsing errors also indicate configuration issues.
	groups, err := ParseGroupsHeader(groupHeader)
	if err != nil {
		requestLogger.Error("Failed to parse group header",
			"header", constant.HeaderGroup,
			"header_value", groupHeader,
			"error", err,
		)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         "Exception thrown while generating token",
			"exceptionCode": "AUTH_FAILURE",
			"refId":         "003",
		})
		c.Abort()
		return
	}

	// Create UserContext from headers and handler config.
	// Tenant comes from TENANT_NAME env var set during maas-api deployment.
	organizationID := strings.TrimSpace(c.GetHeader(constant.HeaderOrganizationID))
	userContext := &UserContext{
		Username:       username,
		Groups:         groups,
		Tenant:         h.tenantName,
		OrganizationID: organizationID,
		AuthSource:     AuthSourceGateway,
	}

	logFields := []any{
		"username", logger.RedactValue(username),
		"groups", groups,
	}
	if organizationID != "" {
		logFields = append(logFields, "organization_id", organizationID)
	}
	requestLogger.Debug("Extracted user info from headers", logFields...)

	c.Set("user", userContext)
	c.Next()
}

// bearerToken extracts a Kubernetes bearer token from the Authorization
// header. Inference API keys (sk-oai-…) are credentials for model routes and
// never authenticate maas-api itself, so they are reported as absent.
func bearerToken(header http.Header) (string, bool) {
	authHeader := strings.TrimSpace(header.Get("Authorization"))
	scheme, value, found := strings.Cut(authHeader, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, constant.APIKeyPrefix) {
		return "", false
	}
	return value, true
}

// authenticateBearerToken implements mechanism 3 for routes whose AuthMode is
// AuthModeGatewayOrKubernetes: with no identity envelope and no gateway
// headers, validate the caller's bearer token via TokenReview. Inference API
// keys are rejected outright — they are credentials for model routes and
// never authenticate maas-api itself. An invalid token is a 401; a TokenReview
// API failure is a 503 so clients can distinguish "bad credentials" from
// "maas-api cannot reach the API server". When required is false (optional
// variant) and no bearer token is present the chain continues without a user
// context.
func (h *Handler) authenticateBearerToken(c *gin.Context, requestLogger *logger.Logger, required bool) {
	if hasInferenceAPIKeyCredential(c.Request.Header) {
		requestLogger.Warn("Rejected inference API key credential on a route that does not accept it")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "inference API keys are not accepted on this route",
		})
		return
	}

	bearer, hasBearer := bearerToken(c.Request.Header)
	if !hasBearer {
		if required {
			requestLogger.Warn("Rejected request without authentication credentials")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":             "unauthorized",
				"exceptionCode":     "AUTH_FAILURE",
				"error_description": "authentication is required",
			})
			return
		}
		requestLogger.Debug("No authentication credentials present, continuing without user context")
		c.Next()
		return
	}

	if h.kubeClient == nil {
		requestLogger.Error("Kubernetes auth fallback requested but no kube client is configured")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error":             "Exception thrown while generating token",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "kubernetes authentication is not configured",
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), TokenReviewTimeout)
	defer cancel()

	user, err := ReviewToken(ctx, h.kubeClient, bearer)
	if errors.Is(err, ErrTokenNotAuthenticated) {
		requestLogger.Warn("Rejected invalid bearer token")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "invalid token",
		})
		return
	}
	if err != nil {
		requestLogger.Error("TokenReview failed", "error", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error":             "authentication unavailable",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "token validation is temporarily unavailable",
		})
		return
	}

	user.Tenant = h.tenantName
	requestLogger.Debug("Authenticated caller via Kubernetes TokenReview",
		"username", logger.RedactValue(user.Username),
	)
	c.Set("user", user)
	c.Next()
}
