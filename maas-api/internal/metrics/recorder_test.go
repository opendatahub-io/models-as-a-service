package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type mockRecorder struct {
	durations   []recordedDuration
	inFlightInc []string
	inFlightDec []string
}

type recordedDuration struct {
	method, route, status string
	duration              time.Duration
}

func (m *mockRecorder) RecordRequestDuration(method, route, status string, d time.Duration) {
	m.durations = append(m.durations, recordedDuration{method, route, status, d})
}

func (m *mockRecorder) IncrementInFlight(method string) {
	m.inFlightInc = append(m.inFlightInc, method)
}

func (m *mockRecorder) DecrementInFlight(method string) {
	m.inFlightDec = append(m.inFlightDec, method)
}

func setupTestRouter(rec MetricsRecorder) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(NewMiddleware(rec))
	r.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.POST("/v1/api-keys", func(c *gin.Context) { c.String(http.StatusCreated, "created") })
	r.GET("/v1/api-keys/:id", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestMiddlewareRecordsRequestDuration(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/models", nil)
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
	req, _ := http.NewRequest("POST", "/v1/api-keys", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, []string{"POST"}, mock.inFlightInc)
	assert.Equal(t, []string{"POST"}, mock.inFlightDec)
}

func TestMiddlewareUsesRoutePatternNotRawPath(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/api-keys/abc-123", nil)
	router.ServeHTTP(w, req)

	assert.Len(t, mock.durations, 1)
	assert.Equal(t, "/v1/api-keys/:id", mock.durations[0].route)
}

func TestMiddlewareUnmatchedRoute(t *testing.T) {
	mock := &mockRecorder{}
	router := setupTestRouter(mock)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/no/such/path", nil)
	router.ServeHTTP(w, req)

	assert.Len(t, mock.durations, 1)
	assert.Equal(t, "unmatched", mock.durations[0].route)
}
