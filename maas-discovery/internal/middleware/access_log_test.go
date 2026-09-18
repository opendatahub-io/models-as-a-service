package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/middleware"
)

func newLogBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return log, &buf
}

func TestAccessLogger_LogsRequest(t *testing.T) {
	log, buf := newLogBuffer()

	r := gin.New()
	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLogger(log))
	r.GET("/v1/tenants", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	assert.Equal(t, "request completed", entry["msg"])
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/v1/tenants", entry["path"])
	assert.InDelta(t, float64(http.StatusOK), entry["status"], 0)
	assert.Contains(t, entry, "latency_ms")
	assert.Contains(t, entry, "client_ip")
	assert.Contains(t, entry, "request_id")
	assert.Equal(t, "INFO", entry["level"])
}

func TestAccessLogger_ProbePathsLogAtDebug(t *testing.T) {
	tests := []struct {
		path string
	}{
		{"/healthz"},
		{"/readyz"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			log, buf := newLogBuffer()

			r := gin.New()
			r.Use(middleware.AccessLogger(log))
			r.GET(tt.path, func(c *gin.Context) { c.Status(http.StatusOK) })

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.ServeHTTP(w, req)

			var entry map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
			assert.Equal(t, "DEBUG", entry["level"])
		})
	}
}

func TestAccessLogger_CapturesStatusCode(t *testing.T) {
	log, buf := newLogBuffer()

	r := gin.New()
	r.Use(middleware.AccessLogger(log))
	r.GET("/error", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	r.ServeHTTP(w, req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.InDelta(t, float64(http.StatusInternalServerError), entry["status"], 0)
}
