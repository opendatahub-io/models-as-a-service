package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/middleware"
)

func TestRequestID_GeneratesWhenMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var captured string
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/test", func(c *gin.Context) {
		captured = middleware.GetRequestID(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.ServeHTTP(w, req)

	assert.NotEmpty(t, captured)
	assert.Equal(t, captured, w.Header().Get(middleware.RequestIDHeader))
}

func TestRequestID_AcceptsValidUpstreamID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var captured string
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/test", func(c *gin.Context) {
		captured = middleware.GetRequestID(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(middleware.RequestIDHeader, "upstream-123.abc_def")
	r.ServeHTTP(w, req)

	assert.Equal(t, "upstream-123.abc_def", captured)
	assert.Equal(t, "upstream-123.abc_def", w.Header().Get(middleware.RequestIDHeader))
}

func TestRequestID_RejectsInvalidCharacters(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"newline injection", "valid\ninjected-header: evil"},
		{"spaces", "has spaces"},
		{"semicolon", "id;drop table"},
		{"angle brackets", "<script>alert(1)</script>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			var captured string
			r := gin.New()
			r.Use(middleware.RequestID())
			r.GET("/test", func(c *gin.Context) {
				captured = middleware.GetRequestID(c)
				c.Status(http.StatusOK)
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set(middleware.RequestIDHeader, tt.id)
			r.ServeHTTP(w, req)

			require.NotEqual(t, tt.id, captured, "invalid ID should be replaced")
			assert.NotEmpty(t, captured, "should generate a replacement ID")
		})
	}
}

func TestRequestID_RejectsTooLong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	longID := make([]byte, 200)
	for i := range longID {
		longID[i] = 'a'
	}

	var captured string
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/test", func(c *gin.Context) {
		captured = middleware.GetRequestID(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(middleware.RequestIDHeader, string(longID))
	r.ServeHTTP(w, req)

	assert.NotEqual(t, string(longID), captured)
	assert.Len(t, captured, 36, "should be a UUID")
}
