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
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
	scopeOrgID    string
}

// newUpstream starts a fake Loki that records the last request and returns a
// successful JSON response.
func newUpstream(t *testing.T) (*httptest.Server, *upstreamRecord) {
	t.Helper()

	record := &upstreamRecord{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record.method = r.Method
		record.path = r.URL.Path
		record.query = r.URL.Query()
		record.authorization = r.Header.Get("Authorization")
		record.scopeOrgID = r.Header.Get("X-Scope-Orgid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(server.Close)
	return server, record
}

// newTLSUpstream starts a fake HTTPS Loki server and returns its CA bundle so
// credential-bearing requests can be tested without sending tokens over HTTP.
func newTLSUpstream(t *testing.T) (*httptest.Server, *upstreamRecord, string) {
	t.Helper()

	record := &upstreamRecord{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record.method = r.Method
		record.path = r.URL.Path
		record.query = r.URL.Query()
		record.authorization = r.Header.Get("Authorization")
		record.scopeOrgID = r.Header.Get("X-Scope-Orgid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(server.Close)

	certificate := server.TLS.Certificates[0]
	caPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificate.Certificate[0],
	}))
	return server, record, caPEM
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
// tests exercising the admin (organization-wide) view. OIDC identities still
// require the organization admins role; tenant-admin SAR checks apply only to
// Kubernetes TokenReview identities.
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

// newK8sRouterWithAdminChecker mirrors newRouterWithAdminChecker, but enables
// the Kubernetes TokenReview fallback so tenant-admin authorization reaches
// the supplied AdminChecker.
func newK8sRouterWithAdminChecker(t *testing.T, checker lokiproxy.AdminChecker) (*gin.Engine, *upstreamRecord) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	upstream, record := newUpstream(t)

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User: authv1.UserInfo{
					Username: "kube-admin@example.com",
					Groups:   []string{"system:authenticated"},
				},
			},
		}, nil
	})

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant").WithKubeClient(client)
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "", "", checker)
	require.NoError(t, err)

	router := gin.New()
	v1 := router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGatewayOrKubernetes))
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

func requireQueryEnforcement(t *testing.T, query, organizationID, username string) {
	t.Helper()

	expr, err := logqlv2.ParseExpr(query)
	require.NoError(t, err)

	selectorCount := 0
	logQueryCount := 0
	filterStages := make([]string, 0, 2)
	if organizationID != "" {
		filterStages = append(filterStages, `organization_id = "`+organizationID+`"`)
	}
	if username != "" {
		filterStages = append(filterStages, `user_id = "`+username+`"`)
	}
	requiredFilter := "| " + strings.Join(filterStages, " and ")

	expr.Walk(func(e any) {
		switch le := e.(type) {
		case *logqlv2.StreamMatcherExpr:
			selectorCount++
			selectorMatchers := make([]string, 0, len(le.Matchers()))
			for _, matcher := range le.Matchers() {
				selectorMatchers = append(selectorMatchers, matcher.String())
			}
			require.Contains(t, selectorMatchers, `tenant_name="test-tenant"`, "stream selector: %s", le.String())
		case *logqlv2.LogQueryExpr:
			logQueryCount++
			require.Contains(t, le.String(), requiredFilter, "log query: %s", le.String())
		}
	})

	require.Positive(t, selectorCount)
	require.Equal(t, selectorCount, logQueryCount)
}

// noOrganizationIdentity builds a verified OIDC identity envelope without any
// organization claim (the parser rejects org_id unless it is a valid UUID and
// forbids org_name/roles without org_id), exercising the tenant-admin
// fallback rules.
func noOrganizationIdentity() string {
	return `{"source":"oidc","claims":{"iss":"https://issuer.example.com","sub":"admin-subject","aud":"maas-api","azp":"maas-client","preferred_username":"` +
		testUsername + `","groups":["ai-grid-users"]}}`
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

	requireQueryEnforcement(t, record.query.Get("query"), testOrganizationID, "")
	require.Contains(t, record.query.Get("query"), `organization_id="other-org"`)
}

func TestProxyQueryEnforcesTenantMatcherOnEveryStreamSelector(t *testing.T) {
	router, record := newAdminRouter(t, "")
	query := `sum(count_over_time({app="api"}[5m])) or sum(count_over_time({app="other"}[5m]))`

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query?query="+url.QueryEscape(query), adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	requireQueryEnforcement(t, record.query.Get("query"), testOrganizationID, "")
}

func TestProxyQueryInstantEnforcesOrganizationMatcher(t *testing.T) {
	router, record := newAdminRouter(t, "/api/logs/v1/application")

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query?query="+url.QueryEscape(`sum(count_over_time({app="api", organization_id="other-org"}[5m]))`)+"&time=1234567890", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, http.MethodGet, record.method)
	require.Equal(t, "/api/logs/v1/application/loki/api/v1/query", record.path)
	require.Equal(t, "1234567890", record.query.Get("time"))

	requireQueryEnforcement(t, record.query.Get("query"), testOrganizationID, "")
	require.Contains(t, record.query.Get("query"), `organization_id="other-org"`)
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

	requireQueryEnforcement(
		t,
		record.query.Get("query"),
		testOrganizationID,
		testUsername,
	)
	require.Contains(t, record.query.Get("query"), `organization_id="other-org"`)
	require.Contains(t, record.query.Get("query"), `user_id="victim"`)
}

func TestProxyLabelsRejectsPostParseIdentityFilters(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?start=1", adminIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyLabelValuesRejectsPostParseIdentityFilters(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/organization_id/values?query="+
		url.QueryEscape(`{organization_id="other"}`), adminIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
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

func TestProxyRejectsEmptyQuery(t *testing.T) {
	router, record := newAdminRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range", adminIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsOversizedQuery(t *testing.T) {
	router, record := newAdminRouter(t, "")
	selector := `{app="api"}`
	query := selector + strings.Repeat(" ", 32*1024+1)

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(query), adminIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(selector), adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/query_range", record.path)
	require.Contains(t, record.query.Get("query"), `app="api"`)
	require.Contains(t, record.query.Get("query"), `tenant_name="test-tenant"`)
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

	requireQueryEnforcement(t, record.query.Get("query"), testOrganizationID, testUsername)
	require.Contains(t, record.query.Get("query"), `organization_id="other-org"`)
	require.Contains(t, record.query.Get("query"), `user_id="victim"`)
}

func TestProxyOnlyUserLabelsRejectPostParseIdentityFilters(t *testing.T) {
	router, record := newRouter(t)

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?only_user=true&start=1", userIdentity())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyOnlyUserAdminKeepsOrganizationWideViewWithoutFlag(t *testing.T) {
	router, record := newAdminRouter(t, "")

	// An admin may query arbitrary user_id values via plain LogQL; only the
	// organization matcher is injected.
	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{user_id="someone-else"}`),
		adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	requireQueryEnforcement(t, record.query.Get("query"), testOrganizationID, "")
	require.Contains(t, record.query.Get("query"), `user_id="someone-else"`)
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
	tests := []struct {
		name     string
		checker  lokiproxy.AdminChecker
		expected int
	}{
		{name: "nil checker denies rather than grants", checker: nil, expected: http.StatusForbidden},
		{name: "checker error is unavailable", checker: stubAdminChecker{err: errors.New("sar unavailable")}, expected: http.StatusServiceUnavailable},
		{name: "successful checker reaches Loki", checker: stubAdminChecker{admin: true}, expected: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, record := newK8sRouterWithAdminChecker(t, tt.checker)
			req := httptest.NewRequest(http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", nil)
			req.Header.Set("Authorization", "Bearer sha256~kube-token")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, tt.expected, w.Code)
			if tt.expected == http.StatusOK {
				require.Contains(t, record.query.Get("query"), `tenant_name="test-tenant"`)
			} else {
				require.Empty(t, record.path, "upstream must not be called")
			}
		})
	}
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

	upstream, record, caPEM := newTLSUpstream(t)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "sa-token-123", caPEM, stubAdminChecker{admin: true})
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "Bearer sa-token-123", record.authorization)
	require.Equal(t, "application", record.scopeOrgID)
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

func TestProxyTokenPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		pathToken     string
		explicitToken string
		expected      string
	}{
		{
			name:          "service account token path",
			pathToken:     " projected-token-123 \n",
			explicitToken: "",
			expected:      "Bearer projected-token-123",
		},
		{
			name:          "explicit token wins",
			pathToken:     "projected-token-123",
			explicitToken: "secret-token-123",
			expected:      "Bearer secret-token-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			tokenPath := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(tokenPath, []byte(tt.pathToken), 0o600))
			upstream, record, caPEM := newTLSUpstream(t)

			tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
			lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, tt.explicitToken, caPEM, stubAdminChecker{admin: true})
			require.NoError(t, err)
			_, err = lokiHandler.WithServiceAccountTokenPath(tokenPath)
			require.NoError(t, err)

			router := gin.New()
			lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

			w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, tt.expected, record.authorization)
		})
	}
}

func TestProxyRejectsUnreadableServiceAccountToken(t *testing.T) {
	tests := []struct {
		name       string
		tokenValue string
	}{
		{name: "missing token file", tokenValue: ""},
		{name: "blank token file", tokenValue: " \n\t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream, record, caPEM := newTLSUpstream(t)
			tokenPath := filepath.Join(t.TempDir(), "token")
			if tt.tokenValue != "" {
				require.NoError(t, os.WriteFile(tokenPath, []byte(tt.tokenValue), 0o600))
			}

			tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
			lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "", caPEM, stubAdminChecker{admin: true})
			require.NoError(t, err)
			_, err = lokiHandler.WithServiceAccountTokenPath(tokenPath)
			require.NoError(t, err)

			router := gin.New()
			lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

			w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

			require.Equal(t, http.StatusBadGateway, w.Code)
			require.Empty(t, record.path, "upstream must not be called")
		})
	}
}

func TestNewHandlerRejectsHTTPWithBearerToken(t *testing.T) {
	_, err := lokiproxy.NewHandler(logger.Production(), "http://loki.example", "token", "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP cannot be used with an upstream bearer token")

	handler, err := lokiproxy.NewHandler(logger.Production(), "http://loki.example", "", "", nil)
	require.NoError(t, err)
	_, err = handler.WithServiceAccountTokenPath("/var/run/secrets/kubernetes.io/serviceaccount/token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP cannot be used with an upstream bearer token")
}

func TestNewHandlerWarnsAboutPlaintextHTTP(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)

	originalStdout := os.Stdout
	t.Cleanup(func() {
		os.Stdout = originalStdout
		_ = writer.Close()
		_ = reader.Close()
	})
	os.Stdout = writer
	log := logger.Production()
	_, err = lokiproxy.NewHandler(log, "http://loki.example", "", "", nil)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	os.Stdout = originalStdout

	output, readErr := io.ReadAll(reader)
	require.NoError(t, readErr)
	require.Contains(t, string(output), "Loki read endpoint uses plaintext HTTP")
}

func TestProxyCanDisableScopeOrgIDForCustomUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream, record, caPEM := newTLSUpstream(t)
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL, "secret-token-123", caPEM, stubAdminChecker{admin: true})
	require.NoError(t, err)
	lokiHandler.WithScopeOrgID("")

	router := gin.New()
	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractVerifiedIdentityOptional(), tokenHandler.ExtractUserInfo(token.AuthModeGateway)))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminIdentity())

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, record.scopeOrgID)
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
		requireQueryEnforcement(t, record.query.Get("query"), "", "kube-admin@example.com")
		require.Contains(t, record.query.Get("query"), `user_id="victim"`)
		require.NotContains(t, record.query.Get("query"), `organization_id="`)
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
		requireQueryEnforcement(t, record.query.Get("query"), "", "kube-admin@example.com")
		require.NotContains(t, record.query.Get("query"), `organization_id="`)
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
		requireQueryEnforcement(
			t,
			record.query.Get("query"),
			testOrganizationID,
			"kube-admin@example.com",
		)
	})
}
