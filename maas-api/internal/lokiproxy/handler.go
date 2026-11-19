package lokiproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

	// onlyUserParam narrows the query to the calling user's own logs (see
	// authorize). It is consumed by the proxy and never forwarded upstream.
	onlyUserParam = "only_user"

	// proxyTimeout bounds a single upstream Loki request.
	proxyTimeout = 60 * time.Second
)

// upstreamPaths maps the maas-api route to the proxied Loki read API path.
const (
	upstreamQuery       = "/loki/api/v1/query"
	upstreamQueryRange  = "/loki/api/v1/query_range"
	upstreamLabels      = "/loki/api/v1/labels"
	upstreamLabelValues = "/loki/api/v1/label/%s/values"
)

// Handler proxies a restricted subset of the Loki read API to an upstream Loki,
// injecting the caller's organization as a label matcher into every query.
type Handler struct {
	logger   *logger.Logger
	upstream *url.URL
	token    string
	client   *http.Client
	// adminChecker resolves tenant admins via SubjectAccessReview
	// (the cached checker wired in cmd/main.go); a nil checker fails closed.
	adminChecker AdminChecker
}

// AdminChecker is an interface for checking if a user is a tenant admin.
// The SARAdminChecker implementation uses Kubernetes SubjectAccessReview
// to check if the user can create maasauthpolicies (RBAC-based admin detection).
type AdminChecker interface {
	IsAdmin(ctx context.Context, user *token.UserContext) (bool, error)
}

// NewHandler creates a Loki proxy handler for the given upstream read endpoint
// (LOKI_READ_ENDPOINT, e.g. https://loki-gateway.example.svc:8080/api/logs/v1/application).
// token (LOKI_READ_TOKEN) is sent as the Authorization: Bearer credential on
// every proxied upstream request; when empty no Authorization header is sent.
// caPEM (LOKI_READ_CA) is a certificate bundle appended to (never replacing)
// the system trust pool used for upstream requests; when empty the system
// pool alone is used. A non-empty caPEM from which no certificate can be
// parsed is rejected so a corrupt bundle fails loudly at startup instead of
// half-verifying some chains. adminChecker backs the tenant-admin check for
// callers without an organization; a nil checker fails closed.
func NewHandler(log *logger.Logger, endpoint, token, caPEM string, adminChecker AdminChecker) (*Handler, error) {
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

	client := &http.Client{Timeout: proxyTimeout}
	if ca := strings.TrimSpace(caPEM); ca != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA pool for Loki read proxy: %w", err)
		}
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("invalid Loki read CA bundle (LOKI_READ_CA): no PEM certificate could be parsed")
		}
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			base = &http.Transport{}
		}
		transport := base.Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
		client.Transport = transport
	}

	return &Handler{
		logger:       log,
		upstream:     upstream,
		token:        strings.TrimSpace(token),
		client:       client,
		adminChecker: adminChecker,
	}, nil
}

// RegisterRoutes registers the read-only Loki proxy routes on the given group.
// Only these four Loki read endpoints are proxied; every other Loki API path
// (writes, rules, tail, series, legacy /api/prom/*) is not registered and
// therefore returns 404.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/logs/loki/api/v1/query", h.proxyHandler(upstreamQuery, true))
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

// authorize resolves the caller's identity from the auth context and returns
// the label matchers that restrict the query, or false after writing an error
// response when the request must not be proxied.
//
// Authorization applies the same stacking rules regardless of which mechanism
// authenticated the caller (OIDC envelope, gateway-injected headers, or the
// Kubernetes TokenReview fallback):
//
//  1. only_user=true applies the user_id filter.
//  2. an organization on the identity applies the organization filter (the
//     verified organization claim for OIDC callers, the gateway-injected
//     organization header otherwise; Kubernetes-authenticated callers never
//     carry one).
//  3. only_user!=true with no organization requires the tenant-admin check
//     (SubjectAccessReview keyed on the username — the OIDC
//     preferred_username for envelope callers); admins get unfiltered,
//     cluster-wide visibility, everyone else is rejected.
//
// Tech debt: any gateway- or TokenReview-authenticated caller is assumed to
// have tenant access; this should become a real SubjectAccessReview check.
func (h *Handler) authorize(c *gin.Context, onlyUser bool) ([]*labels.Matcher, bool) {
	requestLogger := h.requestLogger(c)

	value, ok := c.Get("user")
	if !ok {
		requestLogger.Warn("Rejected Loki proxy request without user context")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "authentication is required",
		})
		return nil, false
	}

	user, ok := value.(*token.UserContext)
	if !ok || user == nil {
		requestLogger.Warn("Rejected Loki proxy request with invalid user context")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error":             "unauthorized",
			"exceptionCode":     "AUTH_FAILURE",
			"error_description": "authentication is required",
		})
		return nil, false
	}

	organizationMatcher := &labels.Matcher{Type: labels.MatchEqual, Name: organizationLabel, Value: user.OrganizationID}
	userMatcher := &labels.Matcher{Type: labels.MatchEqual, Name: userLabel, Value: user.Username}

	// The three rules stack: only_user=true applies the user_id filter (the
	// label carries the username the usage-logs pipeline records for inference
	// traffic); an organization applies the organization filter. With either
	// filter in place the query is scoped and authorized.
	matchers := make([]*labels.Matcher, 0, 2)
	if onlyUser {
		matchers = append(matchers, userMatcher)
	}
	if user.OrganizationID != "" {
		matchers = append(matchers, organizationMatcher)
	}
	if len(matchers) > 0 {
		return matchers, true
	}

	// No organization and no only_user: only tenant admins are authorized and
	// see unfiltered, cluster-wide logs — no tenant→organization mapping
	// exists in the system (organization_id is subscription-scoped).
	isAdmin, ok := h.authorizeTenantAdmin(c, user, requestLogger)
	if !ok {
		return nil, false
	}
	if !isAdmin {
		requestLogger.Warn("Rejected Loki proxy request from non-admin without only_user",
			"username", logger.RedactValue(user.Username),
		)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":             "forbidden",
			"exceptionCode":     "AUTHZ_FAILURE",
			"error_description": "tenant admin access is required, or request only_user=true for your own logs",
		})
		return nil, false
	}

	return nil, true
}

// authorizeTenantAdmin resolves and enforces the tenant-admin requirement,
// writing the error response and returning false when the admin check itself
// fails. A false isAdmin with ok=true is a plain authorization denial the
// caller still has to translate into a 403. Tenant-admin resolution runs the
// injected SubjectAccessReview checker for every caller (keyed on the
// username — the OIDC preferred_username for envelope-authenticated
// callers). A nil checker fails closed.
func (h *Handler) authorizeTenantAdmin(c *gin.Context, user *token.UserContext, requestLogger *logger.Logger) (bool, bool) {
	if h.adminChecker == nil {
		return false, true
	}
	isAdmin, err := h.adminChecker.IsAdmin(
		c.Request.Context(),
		user,
	)
	if err != nil {
		requestLogger.Error("Tenant admin check failed", "error", err)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error":             "authorization unavailable",
			"exceptionCode":     "AUTHZ_FAILURE",
			"error_description": "the tenant admin check failed",
		})
		return false, false
	}
	return isAdmin, true
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

		required, ok := h.authorize(c, onlyUser)
		if !ok {
			return
		}

		// Copy the query parameters so the client's URL is never mutated;
		// everything except "query" is passed through verbatim. The only_user
		// flag is consumed here and never forwarded upstream.
		values := c.Request.URL.Query()
		values.Del(onlyUserParam)

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

		// Authenticate against the upstream Loki with the service account token
		// from the usage-logs-loki Secret (LOKI_READ_TOKEN). The caller's own
		// credentials are never forwarded upstream.
		if h.token != "" {
			req.Header.Set("Authorization", "Bearer "+h.token)
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
