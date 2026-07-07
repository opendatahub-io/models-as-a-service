package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// TestMiddleware_UsesConfiguredTenantRegardlessOfUserContext verifies that the
// metrics middleware always uses the configured default tenant, not the user's.
func TestMiddleware_UsesConfiguredTenantRegardlessOfUserContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := prometheus.NewRegistry()
	recorder, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)

	router := gin.New()
	router.Use(metrics.NewMiddleware(recorder, "models-as-a-service"))
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("user", &token.UserContext{
			Username: "alice",
			Groups:   []string{"users"},
			Tenant:   "redteam",
		})
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	val := gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"method": "GET", "route": "/v1/models", "status": "200", "tenant_name": "models-as-a-service"})
	assert.InDelta(t, float64(1), val, 0)
}

// TestMiddleware_DefaultTenantWhenNoUserContext verifies that requests without
// auth middleware (health, internal) produce metrics with the default tenant label.
func TestMiddleware_DefaultTenantWhenNoUserContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := prometheus.NewRegistry()
	recorder, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)

	router := gin.New()
	router.Use(metrics.NewMiddleware(recorder, "models-as-a-service"))
	router.GET("/health", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	val := gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"method": "GET", "route": "/health", "status": "200", "tenant_name": "models-as-a-service"})
	assert.InDelta(t, float64(1), val, 0)
}
