package lokiproxy_test

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/lokiproxy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const testOrganizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
const testUsername = "admin@example.com"

type upstreamRecord struct {
	method        string
	path          string
	query         url.Values
	authorization string
}

// newUpstream starts a fake Loki that records the last request and replies
// with 200 and a success body.
func newUpstream(t *testing.T) (*httptest.Server, *upstreamRecord) {
	t.Helper()

	record := &upstreamRecord{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record.method = r.Method
		record.path = r.URL.Path
		record.query = r.URL.Query()
		record.authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(server.Close)
	return server, record
}

// stubAdminChecker is a deterministic AdminChecker for tests.
type stubAdminChecker struct {
	admin bool
	err   error
}

func (s stubAdminChecker) IsAdmin(_ context.Context, _ *token.UserContext) (bool, error) {
	return s.admin, s.err
}

// newRouter builds a router that mirrors the production middleware chain for
// the Loki proxy: the gateway-verified identity envelope is parsed first, then
// the trusted identity headers are parsed into the user context (with the
// Kubernetes TokenReview fallback disabled — bearer auth is covered by its own
// tests), then the proxy routes are registered. pathPrefix is appended to the
// fake upstream's URL to exercise base-path concatenation. The stub admin
// checker denies by default so tests fail closed unless they opt in.
func newRouter(t *testing.T) (*gin.Engine, *upstreamRecord) {
	t.Helper()
	return newRouterWithAdminChecker(t, "", stubAdminChecker{})
}

// newAdminRouter is newRouter with a checker that grants tenant admin, for
// tests exercising the admin (organization-wide) view. Since tenant admin is
// resolved via SubjectAccessReview for every caller, OIDC identities alone no
// longer grant the admin view.
func newAdminRouter(t *testing.T, pathPrefix string) (*gin.Engine, *upstreamRecord) {
	t.Helper()
	return newRouterWithAdminChecker(t, pathPrefix, stubAdminChecker{admin: true})
}

func newRouterWithAdminChecker(t *testing.T, pathPrefix string, checker lokiproxy.AdminChecker) (*gin.Engine, *upstreamRecord) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL+pathPrefix, "", "", checker)
	require.NoError(t, err)

	router := gin.New()
	v1 := router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway))
	lokiHandler.RegisterRoutes(v1)

	return router, record
}

// lokiRequest issues a request with the trusted gateway identity envelope.
func lokiRequest(t *testing.T, router *gin.Engine, method, target, identity string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(constant.HeaderIdentity, identity)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// identityEnvelope builds a verified OIDC identity envelope for the test
// organization with the given roles JSON array.
func identityEnvelope(roles string) string {
	return `{"source":"oidc","claims":{"iss":"https://issuer.example.com","sub":"admin-subject","aud":"maas-api","azp":"maas-client","preferred_username":"` +
		testUsername + `","groups":["ai-grid-users"],"org_id":"` +
		testOrganizationID + `","roles":` + roles + `}}`
}

func adminIdentity() string {
	return identityEnvelope(`["admins"]`)
}

func userIdentity() string {
	return identityEnvelope(`[]`)
}

// noOrganizationIdentity builds a verified OIDC identity envelope without any
// organization claim (the parser rejects org_id unless it is a valid UUID and
// forbids org_name/roles without org_id), exercising the tenant-admin
// fallback rules.
func noOrganizationIdentity() string {
	return `{"source":"oidc","claims":{"iss":"https://issuer.example.com","sub":"admin-subject","aud":"maas-api","azp":"maas-client","preferred_username":"` +
		testUsername + `","groups":["ai-grid-users"]}}`
}

// selectorMatchers parses a LogQL expression and returns the matchers of every
// stream selector found in it (sorted for stable comparison).
func selectorMatchers(t *testing.T, query string) []string {
	t.Helper()

	expr, err := logqlv2.ParseExpr(query)
	require.NoError(t, err)

	var matchers []string
	expr.Walk(func(e any) {
		if le, ok := e.(*logqlv2.StreamMatcherExpr); ok {
			for _, m := range le.Matchers() {
				matchers = append(matchers, m.String())
			}
		}
	})
	sort.Strings(matchers)
	return matchers
}

func TestProxyQueryRangeEnforcesOrganizationMatcher(t *testing.T) {
	router, record := newAdminRouter(t, "/api/logs/v1/application")

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api", organization_id="other-org"}`)+"&limit=100", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"status":"success"}`, w.Body.String())
	require.Equal(t, http.MethodGet, record.method)
	require.Equal(t, "/api/logs/v1/application/loki/api/v1/query_range", record.path)
	require.Equal(t, "100", record.query.Get("limit"))

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
}

func TestProxyQueryInstantEnforcesOrganizationMatcher(t *testing.T) {
	router, record := newAdminRouter(t, "/api/logs/v1/application")

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query?query="+url.QueryEscape(`sum(count_over_time({app="api", organization_id="other-org"}[5m]))`)+"&time=1234567890", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, http.MethodGet, record.method)
	require.Equal(t, "/api/logs/v1/application/loki/api/v1/query", record.path)
	require.Equal(t, "1234567890", record.query.Get("time"))

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
}

func TestProxyQueryInstantOnlyUserEnforcesMatchers(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query?only_user=true&query="+
			url.QueryEscape(`sum(count_over_time({app="api", organization_id="other-org", user_id="victim"}[5m]))`),
		userIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/query", record.path)
	require.NotContains(t, record.query, "only_user")

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.Contains(t, matchers, `user_id="admin@example.com"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
	require.NotContains(t, matchers, `user_id="victim"`)
}

func TestProxyLabelsInjectsSelectorWhenQueryAbsent(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?start=1", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/labels", record.path)
	require.Equal(t, "1", record.query.Get("start"))
	require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyLabelValues(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/organization_id/values?query="+
		url.QueryEscape(`{organization_id="other"}`), adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/label/organization_id/values", record.path)
	require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyRejectsInvalidLabelName(t *testing.T) {
	router, _ := newRouter(t)

	// Invalid label name characters are rejected by the handler.
	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/bad!name/values", adminIdentity())
	require.Equal(t, http.StatusBadRequest, w.Code)

	// Path traversal in the label name does not match the route at all.
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/..%2Frules/values", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyRejectsInvalidQuery(t *testing.T) {
	router, _ := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app=`), adminIdentity())
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query?query="+url.QueryEscape(`{app=`), adminIdentity())
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyOnlyUserAuthzMatrix(t *testing.T) {
	query := "?query=" + url.QueryEscape(`{app="api"}`)

	tt := []struct {
		desc     string
		identity string
		admin    bool
		onlyUser string
		expected int
	}{
		{
			desc:     "SAR admin without flag is authorized",
			identity: adminIdentity(),
			admin:    true,
			onlyUser: "",
			expected: http.StatusOK,
		},
		{
			desc:     "SAR admin with flag is authorized and self-scoped",
			identity: adminIdentity(),
			admin:    true,
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "organization user with flag is authorized",
			identity: userIdentity(),
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "organization user without flag is organization-scoped",
			identity: userIdentity(),
			onlyUser: "",
			expected: http.StatusOK,
		},
		{
			// The organization claim scopes the view on its own; the tenant-admin
			// check is only reached without an organization and without only_user.
			desc:     "admins organization role without SAR admin is organization-scoped",
			identity: adminIdentity(),
			onlyUser: "",
			expected: http.StatusOK,
		},
		{
			desc:     "unknown role with flag is authorized as organization member",
			identity: identityEnvelope(`["owner"]`),
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "no organization with flag is authorized and self-scoped",
			identity: noOrganizationIdentity(),
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "no organization without flag is rejected for non-admin",
			identity: noOrganizationIdentity(),
			onlyUser: "",
			expected: http.StatusForbidden,
		},
		{
			desc:     "no organization without flag is authorized for SAR admin",
			identity: noOrganizationIdentity(),
			admin:    true,
			onlyUser: "",
			expected: http.StatusOK,
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: tc.admin})

			target := "/v1/logs/loki/api/v1/query_range" + query
			if tc.onlyUser != "" {
				target += "&only_user=" + tc.onlyUser
			}
			w := lokiRequest(t, router, http.MethodGet, target, tc.identity)

			require.Equal(t, tc.expected, w.Code)
			if tc.expected == http.StatusOK {
				require.Equal(t, "/loki/api/v1/query_range", record.path)
			} else {
				require.Empty(t, record.path, "upstream must not be called")
			}
		})
	}
}

func TestProxyOnlyUserInjectsUserAndOrganizationMatchers(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?only_user=true&query="+url.QueryEscape(`{app="api", organization_id="other-org", user_id="victim"}`),
		userIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, record.query, "only_user", "only_user must not be forwarded upstream")

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.Contains(t, matchers, `user_id="admin@example.com"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
	require.NotContains(t, matchers, `user_id="victim"`)
}

func TestProxyOnlyUserInjectsSelectorWhenQueryAbsent(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?only_user=true&start=1", userIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`, `user_id="admin@example.com"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyOnlyUserAdminKeepsOrganizationWideViewWithoutFlag(t *testing.T) {
	router, record := newAdminRouter(t, "")

	// An admin may query arbitrary user_id values via plain LogQL; only the
	// organization matcher is injected.
	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{user_id="someone-else"}`),
		adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.Contains(t, matchers, `user_id="someone-else"`)
	require.NotContains(t, matchers, `user_id="admin@example.com"`)
}

func TestProxyOnlyUserInvalidFlagValue(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?only_user=yes&query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsNonAdmin(t *testing.T) {
	router, record := newRouter(t)

	// A non-admin organization member without only_user no longer needs the
	// tenant-admin check: the organization claim alone scopes the view.
	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`),
		userIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
	require.NotContains(t, matchers, `user_id=`)
}

func TestProxyRejectsNonAdminWithoutOrganization(t *testing.T) {
	router, record := newRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", nil)
	req.Header.Set(constant.HeaderUsername, "admin@example.com")
	req.Header.Set(constant.HeaderGroup, `["ai-grid-users"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// A gateway-authenticated caller without an organization (the AI Grid
	// OC-token path) follows the k8s authorization rules: only_user or tenant
	// admin. The default stub denies admin, so the organization-wide view is
	// a 403 rather than the historical 401.
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyNilAdminCheckerFailsClosed(t *testing.T) {
	// No admin checker configured: the tenant-admin path must deny rather
	// than grant the unfiltered view.
	router, record := newRouterWithAdminChecker(t, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", nil)
	req.Header.Set(constant.HeaderUsername, "admin@example.com")
	req.Header.Set(constant.HeaderGroup, `["ai-grid-users"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsNonGetAndUnknownPaths(t *testing.T) {
	router, record := newRouter(t)

	// Only GET is accepted.
	w := lokiRequest(t, router, http.MethodPost, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)
	w = lokiRequest(t, router, http.MethodPost, "/v1/logs/loki/api/v1/query?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)

	// Only the registered read endpoints are proxied.
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/series?match=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/tail?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/prom/query", adminIdentity())
	require.Equal(t, http.StatusNotFound, w.Code)

	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyPassesThroughUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"error"}`)
	}))
	t.Cleanup(upstream.Close)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "", "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.JSONEq(t, `{"status":"error"}`, w.Body.String())
}

func TestNewHandlerValidatesEndpoint(t *testing.T) {
	_, err := lokiproxy.NewHandler(logger.Production(), "", "", "", nil)
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "not-a-url", "", "", nil)
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "ftp://loki.example", "", "", nil)
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "https://loki.example/api/logs/v1/application", "", "", nil)
	require.NoError(t, err)
}

func TestProxyWithoutTokenSendsNoAuthorizationHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "", "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, record.authorization)
}

// TestProxyReadsTokenFilePerRequest verifies the token file (LOKI_READ_TOKEN_FILE)
// is read on every request, so a rotated projected ServiceAccount token or
// Secret-mounted token takes effect without a restart.
func TestProxyReadsTokenFilePerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t)

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("token-one"), 0o600))

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, tokenFile, "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "Bearer token-one", record.authorization)

	// Rotate the token; the next request must pick it up.
	require.NoError(t, os.WriteFile(tokenFile, []byte("  token-two \n"), 0o600))
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "Bearer token-two", record.authorization)
}

// TestNewHandlerRejectsMalformedCAFile verifies a CA file whose content is not
// parseable PEM fails loudly at startup, and that whitespace-only content is
// treated as absent.
func TestNewHandlerRejectsMalformedCAFile(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "bad-ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte("not a pem bundle"), 0o600))

	_, err := lokiproxy.NewHandler(logger.Production(), "https://loki.example", "", caFile, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no PEM certificate")

	// Whitespace-only is treated as absent: system pool only, no error.
	emptyFile := filepath.Join(t.TempDir(), "blank-ca.crt")
	require.NoError(t, os.WriteFile(emptyFile, []byte("  \n\t"), 0o600))
	_, err = lokiproxy.NewHandler(logger.Production(), "https://loki.example", "", emptyFile, nil)
	require.NoError(t, err)
}

// TestProxyMissingTokenFileSendsNoAuthorizationHeader verifies a missing token
// file is not startup-fatal: the request proceeds without an Authorization
// header and the failure surfaces as an upstream 401 at query time.
func TestProxyMissingTokenFileSendsNoAuthorizationHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t)

	tokenFile := filepath.Join(t.TempDir(), "does-not-exist")

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, tokenFile, "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, record.authorization)
}

// TestProxyCAFileAppendedToSystemPool verifies that the CA file
// (LOKI_READ_CA_FILE) is appended to (not replacing) the system trust pool —
// the proxy reaches a TLS upstream whose certificate is trusted only through
// the supplied bundle — and that a missing CA file is not startup-fatal.
func TestProxyCAFileAppendedToSystemPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	record := &upstreamRecord{}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record.method = r.Method
		record.path = r.URL.Path
		record.query = r.URL.Query()
		record.authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(tlsServer.Close)

	// The self-signed test certificate is not in the system trust store.
	cert := tlsServer.TLS.Certificates[0]
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	caFile := filepath.Join(t.TempDir(), "service-ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte(caPEM), 0o600))

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")

	// Without the CA the TLS handshake fails and the proxy answers 502.
	withoutCA, err := lokiproxy.NewHandler(logger.Production(), tlsServer.URL, "", "", stubAdminChecker{admin: true})
	require.NoError(t, err)
	router := gin.New()
	withoutCA.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))
	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusBadGateway, w.Code)

	// A missing CA file is skipped: the handler stays enabled.
	_, err = lokiproxy.NewHandler(logger.Production(), tlsServer.URL, "", filepath.Join(t.TempDir(), "absent.crt"), stubAdminChecker{admin: true})
	require.NoError(t, err)

	// With the CA file appended to the system pool the upstream is reachable.
	withCAFile, err := lokiproxy.NewHandler(logger.Production(), tlsServer.URL, "", caFile, stubAdminChecker{admin: true})
	require.NoError(t, err)
	router = gin.New()
	withCAFile.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/query_range", record.path)
}

// headerRequest issues a request authenticated only through the trusted
// gateway identity headers (no OIDC envelope), optionally with an
// organization header.
func headerRequest(router *gin.Engine, target, organizationID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(constant.HeaderUsername, "kube-admin@example.com")
	req.Header.Set(constant.HeaderGroup, `[["cluster-admins"]]`)
	if organizationID != "" {
		req.Header.Set(constant.HeaderOrganizationID, organizationID)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestProxyKubernetesAuthzMatrix covers callers without an OIDC envelope and
// without an organization (the AI Grid OpenShift-token path).
func TestProxyKubernetesAuthzMatrix(t *testing.T) {
	query := "/v1/logs/loki/api/v1/query_range?only_user=true&query=" + url.QueryEscape(`{app="api", user_id="victim"}`)

	t.Run("non-admin with only_user is scoped to own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, query, "")
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Contains(t, matchers, `user_id="kube-admin@example.com"`)
		require.NotContains(t, matchers, `user_id="victim"`)
		require.NotContains(t, matchers, `organization_id=`)
	})

	t.Run("admin without only_user is unfiltered", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`), "")
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Equal(t, []string{`app="api"`}, matchers, "admin view injects no additional matchers")
	})

	t.Run("admin with only_user is still scoped to own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, query, "")
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Contains(t, matchers, `user_id="kube-admin@example.com"`)
		require.NotContains(t, matchers, `organization_id=`)
	})

	t.Run("admin check failure is a 503", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{err: errors.New("sar unavailable")})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", "")
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.Empty(t, record.path, "upstream must not be called")
	})
}

// TestProxyGatewayIdentityWithOrganizationIsOrgScoped covers gateway-injected
// identities that do carry an organization: the organization matcher is
// always injected and scopes the view for admins and non-admins alike; the
// tenant-admin check is not reached.
func TestProxyGatewayIdentityWithOrganizationIsOrgScoped(t *testing.T) {
	t.Run("sar admin gets organization-wide view", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`), testOrganizationID)
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Contains(t, matchers, `organization_id="`+testOrganizationID+`"`)
		require.NotContains(t, matchers, `user_id=`)
	})

	t.Run("sar non-admin without only_user is organization-scoped", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", testOrganizationID)
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`}, matchers)
	})

	t.Run("sar non-admin with only_user is scoped to organization and own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?only_user=true&query=%7B%7D", testOrganizationID)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`, `user_id="kube-admin@example.com"`}, selectorMatchers(t, record.query.Get("query")))
	})
}
