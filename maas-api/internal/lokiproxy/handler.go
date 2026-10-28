package lokiproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const (
	// organizationLabel is the Loki label carrying the caller's organization.
	organizationLabel = "organization_id"

	// userLabel is the Loki label carrying the user that produced a log line.
	userLabel = "user_id"

	// adminGroupSuffix is appended to the organization ID to derive the
	// organization admin group ($org_id/ai-grid-admins per the AI Grid
	// convention).
	adminGroupSuffix = "/ai-grid-admins"

	// userGroupSuffix is appended to the organization ID to derive the
	// organization user group ($org_id/ai-grid-users per the AI Grid
	// convention).
	userGroupSuffix = "/ai-grid-users"

	// onlyUserParam narrows the query to the calling user's own logs (see
	// authorize). It is consumed by the proxy and never forwarded upstream.
	onlyUserParam = "only_user"

	// proxyTimeout bounds a single upstream Loki request.
	proxyTimeout = 60 * time.Second
)

// upstreamPaths maps the maas-api route to the proxied Loki read API path.
const (
	upstreamQueryRange  = "/loki/api/v1/query_range"
	upstreamLabels      = "/loki/api/v1/labels"
	upstreamLabelValues = "/loki/api/v1/label/%s/values"
)

// Handler proxies a restricted subset of the Loki read API to an upstream Loki,
// injecting the caller's organization as a label matcher into every query.
type Handler struct {
	logger   *logger.Logger
	upstream *url.URL
	client   *http.Client
}

// NewHandler creates a Loki proxy handler for the given upstream read endpoint
// (LOKI_READ_ENDPOINT, e.g. https://loki-gateway.example.svc:8080/api/logs/v1/application).
func NewHandler(log *logger.Logger, endpoint string) (*Handler, error) {
	if log == nil {
		log = logger.Production()
	}

	upstream, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("invalid Loki read endpoint %q: %w", endpoint, err)
	}
	switch upstream.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("invalid Loki read endpoint %q: scheme must be http or https", endpoint)
	}
	if upstream.Host == "" {
		return nil, fmt.Errorf("invalid Loki read endpoint %q: host is required", endpoint)
	}

	return &Handler{
		logger:   log,
		upstream: upstream,
		client:   &http.Client{Timeout: proxyTimeout},
	}, nil
}

// RegisterRoutes registers the read-only Loki proxy routes on the given group.
// Only these three Loki read endpoints are proxied; every other Loki API path
// (writes, rules, tail, series, legacy /api/prom/*) is not registered and
// therefore returns 404.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/logs/loki/api/v1/query_range", h.proxyHandler(upstreamQueryRange, true))
	rg.GET("/logs/loki/api/v1/labels", h.proxyHandler(upstreamLabels, false))
	rg.GET("/logs/loki/api/v1/label/:name/values", h.proxyToLabelValues)
}

func (h *Handler) proxyToLabelValues(c *gin.Context) {
	name := c.Param("name")
	// The path parameter becomes part of the upstream URL; only accept valid
	// Loki label names so it cannot be used for path manipulation.
	if !isValidLabelName(name) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":             "invalid label name",
			"exceptionCode":     "INVALID_LABEL_NAME",
			"error_description": "the label name is not valid",
		})
		return
	}
	h.proxyHandler(fmt.Sprintf(upstreamLabelValues, name), false)(c)
}

func isValidLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !isLabelNameRune(r, i > 0) {
			return false
		}
	}
	return true
}

func isLabelNameRune(r rune, allowDigit bool) bool {
	switch {
	case r == '_' || isAlpha(r):
		return true
	case isDigit(r):
		return allowDigit
	default:
		return false
	}
}

func isAlpha(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// authorize resolves the caller's organization from the auth context and
// verifies group membership according to the requested view. Without the
// only_user flag the caller must be a member of the organization admin group
// ($org_id/ai-grid-admins) and gets organization-wide visibility. With the
// flag the caller must be a member of the organization user group
// ($org_id/ai-grid-users) — or the admin group, since admins need both views —
// and the returned username restricts results to the caller's own logs. It
// writes the error response and returns false when the request must not be
// proxied.
func (h *Handler) authorize(c *gin.Context, onlyUser bool) (string, string, bool) {
	requestLogger := h.requestLogger(c)

	value, ok := c.Get("user")
	if !ok {
		requestLogger.Warn("Rejected Loki proxy request without user context")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "authentication is required",
		})
		return "", "", false
	}

	user, ok := value.(*token.UserContext)
	if !ok || user == nil || user.OrganizationID == "" {
		requestLogger.Warn("Rejected Loki proxy request without organization ID")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "organization ID is required",
		})
		return "", "", false
	}

	adminGroup := user.OrganizationID + adminGroupSuffix
	isAdmin := containsGroup(user.Groups, adminGroup)

	if onlyUser {
		// The user view accepts both organization users and admins, but always
		// restricts results to the caller's own user_id (injected below).
		userGroup := user.OrganizationID + userGroupSuffix
		if !isAdmin && !containsGroup(user.Groups, userGroup) {
			requestLogger.Warn("Rejected Loki proxy request from user outside the organization user group",
				"username", logger.RedactValue(user.Username),
			)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":             "forbidden",
				"exceptionCode":     "AUTHZ_FAILURE",
				"error_description": "organization membership is required",
			})
			return "", "", false
		}

		// The user_id label on usage logs carries the value of X-MaaS-Username
		// (injected by the usage-logs EnvoyFilter), so the matcher must use the
		// username from the trusted auth-gateway header. ExtractUserInfo
		// guarantees it is present.
		return user.OrganizationID, user.Username, true
	}

	if !isAdmin {
		requestLogger.Warn("Rejected Loki proxy request from non-admin user",
			"username", logger.RedactValue(user.Username),
		)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":             "forbidden",
			"exceptionCode":     "AUTHZ_FAILURE",
			"error_description": "organization admin membership is required",
		})
		return "", "", false
	}

	return user.OrganizationID, "", true
}

// proxyHandler returns a gin handler that enforces the organization matcher and
// forwards the request to the given upstream Loki path. When queryEndpoint is
// true an empty query parameter is passed through untouched (Loki rejects it).
func (h *Handler) proxyHandler(upstreamPath string, queryEndpoint bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestLogger := h.requestLogger(c)

		// only_user=true narrows the query to the caller's own logs and lowers
		// the authorization bar to the organization user group. Any other value
		// keeps the admin-only organization-wide view.
		onlyUser := false
		if raw := c.Query(onlyUserParam); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				requestLogger.Warn("Rejected Loki proxy request with invalid only_user value", "value", raw)
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error":             "invalid only_user parameter",
					"exceptionCode":     "INVALID_PARAMETER",
					"error_description": "the only_user parameter must be a boolean",
				})
				return
			}
			onlyUser = parsed
		}

		organizationID, scopedUsername, ok := h.authorize(c, onlyUser)
		if !ok {
			return
		}

		// Copy the query parameters so the client's URL is never mutated;
		// everything except "query" is passed through verbatim. The only_user
		// flag is consumed here and never forwarded upstream.
		values := c.Request.URL.Query()
		values.Del(onlyUserParam)
		required := []*labels.Matcher{
			{Type: labels.MatchEqual, Name: organizationLabel, Value: organizationID},
		}
		if onlyUser {
			required = append(required, &labels.Matcher{
				Type: labels.MatchEqual, Name: userLabel, Value: scopedUsername,
			})
		}

		if err := enforceQueryMatchers(values, required, queryEndpoint); err != nil {
			requestLogger.Warn("Rejected Loki proxy request with invalid query", "error", err)
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error":             "invalid query",
				"exceptionCode":     "INVALID_QUERY",
				"error_description": "the query parameter is not a valid LogQL expression",
			})
			return
		}

		upstreamURL := *h.upstream
		upstreamURL.Path = strings.TrimSuffix(upstreamURL.Path, "/") + upstreamPath
		upstreamURL.RawQuery = values.Encode()

		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, upstreamURL.String(), nil)
		if err != nil {
			h.proxyError(c, err)
			return
		}

		resp, err := h.client.Do(req)
		if err != nil {
			h.proxyError(c, err)
			return
		}
		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				requestLogger.Debug("Failed to close upstream Loki response body", "error", cerr)
			}
		}()

		// Pass the upstream status, content type and body through verbatim.
		if contentType := resp.Header.Get("Content-Type"); contentType != "" {
			c.Header("Content-Type", contentType)
		}
		c.Status(resp.StatusCode)
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			requestLogger.Error("Failed to stream upstream Loki response", "error", err)
		}
	}
}

func (h *Handler) proxyError(c *gin.Context, err error) {
	h.requestLogger(c).Error("Loki proxy request failed", "error", err)
	c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{
		"error":             "loki proxy failure",
		"exceptionCode":     "UPSTREAM_FAILURE",
		"error_description": "the upstream Loki request failed",
	})
}

func (h *Handler) requestLogger(c *gin.Context) *logger.Logger {
	if requestLogger := middleware.GetLogger(c); requestLogger != nil {
		return requestLogger
	}
	return h.logger.WithContext(c.Request.Context())
}

func containsGroup(groups []string, group string) bool {
	return slices.Contains(groups, group)
}
