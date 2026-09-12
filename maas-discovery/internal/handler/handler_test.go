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
}

func (f *fakeTenantCache) List() []types.TenantInfo {
	return f.tenants
}

func TestListTenantsWithData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fc := &fakeTenantCache{
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
