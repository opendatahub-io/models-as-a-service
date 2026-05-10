package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
)

func TestMetricsServerIntegration(t *testing.T) {
	reg := prometheus.NewRegistry()
	recorder, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := tcpAddr.Port
	require.NoError(t, listener.Close())

	srv, err := metrics.NewMetricsServer(fmt.Sprintf(":%d", port), reg)
	require.NoError(t, err)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Logf("metrics server error: %v", err)
		}
	}()
	t.Cleanup(func() { srv.Close() })

	client := &http.Client{Timeout: 2 * time.Second}

	require.Eventually(t, func() bool {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 50*time.Millisecond)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.NewMiddleware(recorder))
	router.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	scrapeReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	resp, err := client.Do(scrapeReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	assert.Contains(t, bodyStr, `maas_api_http_requests_total{method="GET",route="/v1/models",status="200"} 1`)
	assert.Contains(t, bodyStr, "maas_api_http_request_duration_seconds")
}
