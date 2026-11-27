package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/metrics"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/middleware"
)

func newTestRouter(m *metrics.Metrics) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.Prometheus(m))
	r.GET("/v1/tenants", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doRequest(r *gin.Engine, path string) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
}

func metricValue(t *testing.T, m *metrics.Metrics, name string) (float64, bool) {
	t.Helper()
	families, err := m.Registry().Gather()
	assert.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == name {
			var v float64
			mm := mf.GetMetric()[0]
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				v = mm.GetCounter().GetValue()
			case dto.MetricType_HISTOGRAM:
				v = float64(mm.GetHistogram().GetSampleCount())
			default:
				v = mm.GetGauge().GetValue()
			}
			return v, true
		}
	}
	return 0, false
}

func TestPrometheusMiddleware_InstrumentsAPIRequests(t *testing.T) {
	m := metrics.New()
	r := newTestRouter(m)

	doRequest(r, "/v1/tenants")
	doRequest(r, "/v1/tenants")

	count, ok := metricValue(t, m, "discovery_request_total")
	assert.True(t, ok)
	assert.InDelta(t, 2.0, count, 1e-9)

	hist, ok := metricValue(t, m, "discovery_request_duration_seconds")
	assert.True(t, ok)
	assert.InDelta(t, 2.0, hist, 1e-9)
}

func TestPrometheusMiddleware_SkipsProbePaths(t *testing.T) {
	m := metrics.New()
	r := newTestRouter(m)

	doRequest(r, "/healthz")
	doRequest(r, "/readyz")

	_, ok := metricValue(t, m, "discovery_request_total")
	assert.False(t, ok, "probe paths must not pollute request metrics")
}

func TestPrometheusMiddleware_NilMetricsIsSafe(t *testing.T) {
	r := newTestRouter(nil)
	doRequest(r, "/v1/tenants")
}
