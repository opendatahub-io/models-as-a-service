package metrics

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsServerIntegration(t *testing.T) {
	reg := prometheus.NewRegistry()
	recorder, err := NewPrometheusRecorder(reg)
	require.NoError(t, err)

	// Find a free port for the metrics server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	srv, err := NewMetricsServer(fmt.Sprintf(":%d", port), reg)
	require.NoError(t, err)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Logf("metrics server error: %v", err)
		}
	}()
	t.Cleanup(func() { srv.Close() })

	// Wait for the server to be ready
	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 50*time.Millisecond)

	// Set up a Gin router with the metrics middleware and make a request
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(NewMiddleware(recorder))
	router.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/models", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Scrape the metrics endpoint and verify content
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	assert.Contains(t, bodyStr, `maas_api_http_requests_total{method="GET",route="/v1/models",status="200"} 1`)
	assert.True(t, strings.Contains(bodyStr, "maas_api_http_request_duration_seconds"), "histogram metric missing")
}
