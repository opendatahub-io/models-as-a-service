package metrics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
)

type mockRecorder struct {
	durations   []recordedDuration
	requests    []recordedRequest
	rejections  []string
	inFlightInc []string
	inFlightDec []string
}

type recordedDuration struct {
	method, route, status, tenant string
	duration                      time.Duration
}

type recordedRequest struct {
	duration time.Duration
}

func (m *mockRecorder) RecordRequestDuration(method, route, status, tenant string, d time.Duration) {
	m.durations = append(m.durations, recordedDuration{method, route, status, tenant, d})
}

func (m *mockRecorder) RecordRequest(d time.Duration) {
	m.requests = append(m.requests, recordedRequest{d})
}

func (m *mockRecorder) RecordRejection(reason string) {
	m.rejections = append(m.rejections, reason)
}

func (m *mockRecorder) IncrementInFlight(method string) {
	m.inFlightInc = append(m.inFlightInc, method)
}

func (m *mockRecorder) DecrementInFlight(method string) {
	m.inFlightDec = append(m.inFlightDec, method)
}

func (m *mockRecorder) RecordKeyValidation(tenant, result string) {
}

func (m *mockRecorder) RecordTokenMint(tenant, result string) {
}

func setupTestRouter(rec metrics.MetricsRecorder) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(metrics.NewMiddleware(rec, "test-tenant"))
	r.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.POST("/v1/api-keys", func(c *gin.Context) { c.String(http.StatusCreated, "created") })
	r.GET("/v1/api-keys/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestMiddlewareRecordsRequestDuration(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, mock.durations, 1)
	assert.Equal(t, "GET", mock.durations[0].method)
	assert.Equal(t, "/v1/models", mock.durations[0].route)
	assert.Equal(t, "200", mock.durations[0].status)
	assert.Greater(t, mock.durations[0].duration, time.Duration(0))
}

func TestMiddlewareRecordsInFlight(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/api-keys", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, []string{"POST"}, mock.inFlightInc)
	assert.Equal(t, []string{"POST"}, mock.inFlightDec)
}

func TestMiddlewareUsesRoutePatternNotRawPath(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/api-keys/abc-123", nil)
	router.ServeHTTP(w, req)

	assert.Len(t, mock.durations, 1)
	assert.Equal(t, "/v1/api-keys/:id", mock.durations[0].route)
}

func TestMiddlewareUnmatchedRoute(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/no/such/path", nil)
	router.ServeHTTP(w, req)

	assert.Len(t, mock.durations, 1)
	assert.Equal(t, "unmatched", mock.durations[0].route)
}

func TestMiddlewareRecordsRequest(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, mock.requests, 1)
	assert.Greater(t, mock.requests[0].duration, time.Duration(0))
}
