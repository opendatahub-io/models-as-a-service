package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/handler"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := handler.New(cache.NewStub())
	r := gin.New()
	h.RegisterRoutes(r)
	return r
}

func TestListTenants(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp types.TenantsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Tenants)
	assert.JSONEq(t, `{"tenants":[]}`, w.Body.String())
}

func TestHealthz(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status":"ok"}`, w.Body.String())
}

func TestReadyz(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status":"ok"}`, w.Body.String())
}

func TestReadyzNotReady(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{synced: false}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "not ready")
}

func TestNotFound(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMethodNotAllowed(t *testing.T) {
	r := newTestRouter()
	r.HandleMethodNotAllowed = true

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

type fakeTenantCache struct {
	tenants []types.TenantInfo
	synced  bool
}

func (f *fakeTenantCache) List() []types.TenantInfo {
	return f.tenants
}

func (f *fakeTenantCache) Synced() bool {
	return f.synced
}

func TestListTenantsWithData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{
		synced: true,
		tenants: []types.TenantInfo{
			{
				Name: "test-tenant",
				Gateway: types.GatewayMetadata{
					Name:        "gw-1",
					Namespace:   "ns-1",
					Protocol:    "https",
					ExternalURL: "https://gw.example.com",
					Port:        443,
				},
			},
		},
	}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp types.TenantsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tenants, 1)
	assert.Equal(t, "test-tenant", resp.Tenants[0].Name)
	assert.Equal(t, "gw-1", resp.Tenants[0].Gateway.Name)
}

// Contract tests validate the response schema matches ADR ODH-ADR-MS-0004.

func TestContract_ResponseSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{
		synced: true,
		tenants: []types.TenantInfo{
			{
				Name: "tenant-a",
				Gateway: types.GatewayMetadata{
					Name:        "maas-tenant-a-gateway",
					Namespace:   "openshift-ingress",
					Protocol:    "https",
					ExternalURL: "https://tenant-a.apps.example.com",
					Port:        443,
				},
			},
		},
	}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))

	tenantsRaw, ok := raw["tenants"].([]any)
	require.True(t, ok, "response must have 'tenants' array at top level")
	require.Len(t, tenantsRaw, 1)

	tenant, ok := tenantsRaw[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "tenant-a", tenant["name"])

	gw, ok := tenant["gateway"].(map[string]any)
	require.True(t, ok, "tenant must have 'gateway' object")
	assert.Equal(t, "maas-tenant-a-gateway", gw["name"])
	assert.Equal(t, "openshift-ingress", gw["namespace"])
	assert.Equal(t, "https", gw["protocol"])
	assert.Equal(t, "https://tenant-a.apps.example.com", gw["externalUrl"])
	assert.InDelta(t, float64(443), gw["port"], 0)
}

func TestContract_EmptyTenants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{synced: true}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"tenants":[]}`, w.Body.String(),
		"empty cluster must return empty array, not null")
}

func TestContract_PartialGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{
		synced: true,
		tenants: []types.TenantInfo{
			{
				Name: "degraded-tenant",
				Gateway: types.GatewayMetadata{
					Name:      "missing-gw",
					Namespace: "openshift-ingress",
				},
			},
		},
	}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))

	tenantsRaw, ok := raw["tenants"].([]any)
	require.True(t, ok)
	tenant, ok := tenantsRaw[0].(map[string]any)
	require.True(t, ok)
	gw, ok := tenant["gateway"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "missing-gw", gw["name"])
	assert.Equal(t, "openshift-ingress", gw["namespace"])
	assert.Empty(t, gw["protocol"], "zero-value fields must be present, not omitted")
	assert.Empty(t, gw["externalUrl"])
	assert.InDelta(t, float64(0), gw["port"], 0)
}

func TestContract_MultiTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{
		synced: true,
		tenants: []types.TenantInfo{
			{
				Name: "alpha",
				Gateway: types.GatewayMetadata{
					Name:        "alpha-gw",
					Namespace:   "openshift-ingress",
					Protocol:    "https",
					ExternalURL: "https://alpha.example.com",
					Port:        443,
				},
			},
			{
				Name: "beta",
				Gateway: types.GatewayMetadata{
					Name:        "beta-gw",
					Namespace:   "openshift-ingress",
					Protocol:    "http",
					ExternalURL: "http://beta.example.com",
					Port:        80,
				},
			},
		},
	}
	h := handler.New(fc)
	r := gin.New()
	h.RegisterRoutes(r)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp types.TenantsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tenants, 2)
	assert.Equal(t, "alpha", resp.Tenants[0].Name)
	assert.Equal(t, "beta", resp.Tenants[1].Name)
	assert.Equal(t, "https", resp.Tenants[0].Gateway.Protocol)
	assert.Equal(t, "http", resp.Tenants[1].Gateway.Protocol)
}
