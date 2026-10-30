package lokiproxy_test

import (
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

const testOrganizationID = "1234"
const testUsername = "admin@example.com"

type upstreamRecord struct {
	method string
	path   string
	query  url.Values
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, record
}

// newRouter builds a router that mirrors the production middleware chain for
// the Loki proxy: the trusted identity headers are parsed into the user
// context, then the proxy routes are registered. pathPrefix is appended to the
// fake upstream's URL to exercise base-path concatenation.
func newRouter(t *testing.T, pathPrefix string) (*gin.Engine, *upstreamRecord) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	upstream, record := newUpstream(t, http.StatusOK, `{"status":"success"}`)

	tokenHandler := token.NewHandler(logger.Production(), "test-tenant")
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL+pathPrefix)
	require.NoError(t, err)

	router := gin.New()
	v1 := router.Group("/v1", tokenHandler.ExtractUserInfo())
	lokiHandler.RegisterRoutes(v1)

	return router, record
}

// lokiRequest issues a request with the trusted auth-gateway identity headers.
func lokiRequest(t *testing.T, router *gin.Engine, method, target, groups string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(constant.HeaderUsername, testUsername)
	req.Header.Set(constant.HeaderGroup, groups)
	req.Header.Set(constant.HeaderOrganizationID, testOrganizationID)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func adminGroups() string {
	return `["` + testOrganizationID + `/ai-grid-admins", "other-group"]`
}

func userGroups() string {
	return `["` + testOrganizationID + `/ai-grid-users", "other-group"]`
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
	router, record := newRouter(t, "/api/logs/v1/application")

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api", organization_id="other-org"}`)+"&limit=100", adminGroups())

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"status":"success"}`, w.Body.String())
	require.Equal(t, http.MethodGet, record.method)
	require.Equal(t, "/api/logs/v1/application/loki/api/v1/query_range", record.path)
	require.Equal(t, "100", record.query.Get("limit"))

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="1234"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
}

func TestProxyLabelsInjectsSelectorWhenQueryAbsent(t *testing.T) {
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?start=1", adminGroups())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/labels", record.path)
	require.Equal(t, "1", record.query.Get("start"))
	require.Equal(t, []string{`organization_id="1234"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyLabelValues(t *testing.T) {
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/organization_id/values?query="+
		url.QueryEscape(`{organization_id="other"}`), adminGroups())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "/loki/api/v1/label/organization_id/values", record.path)
	require.Equal(t, []string{`organization_id="1234"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyRejectsInvalidLabelName(t *testing.T) {
	router, _ := newRouter(t, "")

	// Invalid label name characters are rejected by the handler.
	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/bad!name/values", adminGroups())
	require.Equal(t, http.StatusBadRequest, w.Code)

	// Path traversal in the label name does not match the route at all.
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/label/..%2Frules/values", adminGroups())
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyRejectsInvalidQuery(t *testing.T) {
	router, _ := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app=`), adminGroups())
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyOnlyUserAuthzMatrix(t *testing.T) {
	query := "?query=" + url.QueryEscape(`{app="api"}`)

	tt := []struct {
		desc     string
		groups   string
		onlyUser string
		expected int
	}{
		{
			desc:     "admin without flag is authorized",
			groups:   adminGroups(),
			onlyUser: "",
			expected: http.StatusOK,
		},
		{
			desc:     "admin with flag is authorized and self-scoped",
			groups:   adminGroups(),
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "ai-grid-users member with flag is authorized",
			groups:   userGroups(),
			onlyUser: "true",
			expected: http.StatusOK,
		},
		{
			desc:     "ai-grid-users member without flag is rejected",
			groups:   userGroups(),
			onlyUser: "",
			expected: http.StatusForbidden,
		},
		{
			desc:     "member of neither group is rejected without flag",
			groups:   `["some-other-group"]`,
			onlyUser: "",
			expected: http.StatusForbidden,
		},
		{
			desc:     "member of neither group is rejected with flag",
			groups:   `["some-other-group"]`,
			onlyUser: "true",
			expected: http.StatusForbidden,
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			router, record := newRouter(t, "")

			target := "/v1/logs/loki/api/v1/query_range" + query
			if tc.onlyUser != "" {
				target += "&only_user=" + tc.onlyUser
			}
			w := lokiRequest(t, router, http.MethodGet, target, tc.groups)

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
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?only_user=true&query="+url.QueryEscape(`{app="api", organization_id="other-org", user_id="victim"}`),
		userGroups())

	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, record.query, "only_user", "only_user must not be forwarded upstream")

	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="1234"`)
	require.Contains(t, matchers, `user_id="admin@example.com"`)
	require.NotContains(t, matchers, `organization_id="other-org"`)
	require.NotContains(t, matchers, `user_id="victim"`)
}

func TestProxyOnlyUserInjectsSelectorWhenQueryAbsent(t *testing.T) {
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/labels?only_user=true&start=1", userGroups())

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{`organization_id="1234"`, `user_id="admin@example.com"`}, selectorMatchers(t, record.query.Get("query")))
}

func TestProxyOnlyUserAdminKeepsOrganizationWideViewWithoutFlag(t *testing.T) {
	router, record := newRouter(t, "")

	// An admin may query arbitrary user_id values via plain LogQL; only the
	// organization matcher is injected.
	w := lokiRequest(t, router, http.MethodGet,
		"/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{user_id="someone-else"}`),
		adminGroups())

	require.Equal(t, http.StatusOK, w.Code)
	matchers := selectorMatchers(t, record.query.Get("query"))
	require.Contains(t, matchers, `organization_id="1234"`)
	require.Contains(t, matchers, `user_id="someone-else"`)
	require.NotContains(t, matchers, `user_id="admin@example.com"`)
}

func TestProxyOnlyUserInvalidFlagValue(t *testing.T) {
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?only_user=yes&query=%7B%7D", adminGroups())

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsNonAdmin(t *testing.T) {
	router, record := newRouter(t, "")

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query="+url.QueryEscape(`{app="api"}`),
		`["some-other-group"]`)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsMissingOrganization(t *testing.T) {
	router, record := newRouter(t, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", nil)
	req.Header.Set(constant.HeaderUsername, "admin@example.com")
	req.Header.Set(constant.HeaderGroup, adminGroups())

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Empty(t, record.path, "upstream must not be called")
}

func TestProxyRejectsNonGetAndUnknownPaths(t *testing.T) {
	router, record := newRouter(t, "")

	// Only GET is accepted.
	w := lokiRequest(t, router, http.MethodPost, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminGroups())
	require.Equal(t, http.StatusNotFound, w.Code)

	// Only the registered read endpoints are proxied.
	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/series?match=%7B%7D", adminGroups())
	require.Equal(t, http.StatusNotFound, w.Code)

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/tail?query=%7B%7D", adminGroups())
	require.Equal(t, http.StatusNotFound, w.Code)

	w = lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/prom/query", adminGroups())
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
	lokiHandler, err := lokiproxy.NewHandler(logger.Production(), upstream.URL)
	require.NoError(t, err)

	router := gin.New()
	lokiHandler.RegisterRoutes(router.Group("/v1", tokenHandler.ExtractUserInfo()))

	w := lokiRequest(t, router, http.MethodGet, "/v1/logs/loki/api/v1/query_range?query=%7B%7D", adminGroups())

	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.JSONEq(t, `{"status":"error"}`, w.Body.String())
}

func TestNewHandlerValidatesEndpoint(t *testing.T) {
	_, err := lokiproxy.NewHandler(logger.Production(), "")
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "not-a-url")
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "ftp://loki.example")
	require.Error(t, err)

	_, err = lokiproxy.NewHandler(logger.Production(), "https://loki.example/api/logs/v1/application")
	require.NoError(t, err)
}
