package lokiproxy_test

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// newUpstream starts a fake Loki that records the last request and replies with
// the given status and body.
func newUpstream(t *testing.T, status int, body string) (*httptest.Server, *upstreamRecord) {
	t.Helper()

	record := &upstreamRecord{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record.method = r.Method
		record.path = r.URL.Path
		record.query = r.URL.Query()
		record.authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
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

	upstream, record := newUpstream(t, http.StatusOK, `{"status":"success"}`)

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
	require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`, `tenant_name="test-tenant"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyLabelValues(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/organization_id/values?query="+
		url.QueryEscape(`{organization_id="other"}`), adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/label/organization_id/values", record.path)
	require.Equal(t, []string{`organization_id="` + testOrganizationID + `"`, `tenant_name="test-tenant"`}, selectorMatchers(t, record.query.Get("query")))
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
			// Without only_user an organization member needs the admins role
			// for the organization-wide view.
			desc:     "organization user without flag is rejected without the admins role",
			identity: userIdentity(),
			onlyUser: "",
			expected: http.StatusForbidden,
		},
		{
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
			desc:     "no organization without flag is rejected for non-k8s SAR admin",
			identity: noOrganizationIdentity(),
			admin:    true,
			onlyUser: "",
			expected: http.StatusForbidden,
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
	require.Equal(t, []string{
		`organization_id="` + testOrganizationID + `"`,
		`tenant_name="test-tenant"`,
		`user_id="admin@example.com"`,
	}, selectorMatchers(t, record.query.Get("query")))
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

// TestProxyRejectsNonAdmin verifies that an organization member without the
// admins role cannot get the organization-wide view without only_user.
func TestProxyRejectsNonAdmin(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`),
		userIdentity())

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
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

func TestProxyInjectsUpstreamBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t, http.StatusOK, `{"status":"success"}`)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "sa-token-123", "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "Bearer sa-token-123", record.authorization)
}

func TestProxyWithoutTokenSendsNoAuthorizationHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t, http.StatusOK, `{"status":"success"}`)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "", "", stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, record.authorization)
}

func TestNewHandlerRejectsMalformedCA(t *testing.T) {
	_, err := lokiproxy.NewHandler(logger.Production(), "https://loki.example", "", "not a pem bundle", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "LOKI_READ_CA")

	// Whitespace-only is treated as absent: system pool only, no error.
	_, err = lokiproxy.NewHandler(logger.Production(), "https://loki.example", "", "  \n\t", nil)
	require.NoError(t, err)
}

// TestProxyCAAppendedToSystemPool verifies that a PEM bundle passed as
// LOKI_READ_CA is appended to (not replacing) the system trust pool: the
// proxy reaches a TLS upstream whose certificate is trusted only through the
// supplied bundle.
func TestProxyCAAppendedToSystemPool(t *testing.T) {
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

	// Without the CA the TLS handshake fails and the proxy answers 502.
	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	withoutCA, err := lokiproxy.NewHandler(logger.Production(), tlsServer.URL, "", "", stubAdminChecker{admin: true})
	require.NoError(t, err)
	router := gin.New()
	withoutCA.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))
	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())
	require.Equal(t, http.StatusBadGateway, w.Code)

	// With the CA appended to the system pool the upstream is reachable.
	withCA, err := lokiproxy.NewHandler(logger.Production(), tlsServer.URL, "", caPEM, stubAdminChecker{admin: true})
	require.NoError(t, err)
	router = gin.New()
	withCA.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))
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
	req.Header.Set(constant.HeaderGroup, `["cluster-admins"]`)
	if organizationID != "" {
		req.Header.Set(constant.HeaderOrganizationID, organizationID)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestProxyKubernetesAuthzMatrix covers callers without an OIDC envelope and
// without an organization (the AI Grid gateway-header path). Gateway-authenticated
// callers are never tenant admins; only the only_user self-scoped view is allowed.
func TestProxyKubernetesAuthzMatrix(t *testing.T) {
	query := "/v1/logs/loki/api/v1/query_range?only_user=true&query=" + url.QueryEscape(`{app="api", user_id="victim"}`)

	t.Run("non-admin with only_user is scoped to own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, query, "")
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Contains(t, matchers, `tenant_name="test-tenant"`)
		require.Contains(t, matchers, `user_id="kube-admin@example.com"`)
		require.NotContains(t, matchers, `user_id="victim"`)
		require.NotContains(t, matchers, `organization_id=`)
	})

	t.Run("gateway admin without only_user is rejected (non-k8s auth source)", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`), "")
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Empty(t, record.path, "upstream must not be called")
	})

	t.Run("gateway admin with only_user is still scoped to own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, query, "")
		require.Equal(t, http.StatusOK, w.Code)
		matchers := selectorMatchers(t, record.query.Get("query"))
		require.Contains(t, matchers, `user_id="kube-admin@example.com"`)
		require.NotContains(t, matchers, `organization_id=`)
	})

	t.Run("gateway admin check failure is never reached (non-k8s auth source)", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{err: errors.New("sar unavailable")})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", "")
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Empty(t, record.path, "upstream must not be called")
	})
}

// TestProxyGatewayIdentityWithOrganizationIsOrgScoped covers gateway-injected
// identities that do carry an organization: the organization-wide view needs
// the admins role (which gateway-authenticated callers cannot hold), while
// only_user scopes the query to the caller's own organization logs.
func TestProxyGatewayIdentityWithOrganizationIsOrgScoped(t *testing.T) {
	t.Run("sar admin without only_user is rejected without the admins role", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: true})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`), testOrganizationID)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Empty(t, record.path, "upstream must not be called")
	})

	t.Run("sar non-admin without only_user is rejected without the admins role", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", testOrganizationID)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Empty(t, record.path, "upstream must not be called")
	})

	t.Run("non-admin with only_user is scoped to organization and own user_id", func(t *testing.T) {
		router, record := newRouterWithAdminChecker(t, "", stubAdminChecker{admin: false})
		w := headerRequest(router, "/v1/logs/loki/api/v1/query_range?only_user=true&query=%7B%7D", testOrganizationID)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, []string{
			`organization_id="` + testOrganizationID + `"`,
			`tenant_name="test-tenant"`,
			`user_id="kube-admin@example.com"`,
		}, selectorMatchers(t, record.query.Get("query")))
	})
}
