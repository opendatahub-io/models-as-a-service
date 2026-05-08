package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
)

func TestRequestID_GeneratesUUIDWhenMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID())

	var capturedID string
	router.GET("/test", func(c *gin.Context) {
		capturedID = middleware.GetRequestID(c)
		c.Status(200)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.NotEmpty(t, capturedID, "Should generate request ID")
	assert.Equal(t, capturedID, w.Header().Get("X-Request-ID"),
		"Response header should match context value")
}

func TestRequestID_UsesValidClientSuppliedID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID())

	var capturedID string
	router.GET("/test", func(c *gin.Context) {
		capturedID = middleware.GetRequestID(c)
		c.Status(200)
	})

	clientID := "client-request-id-123"
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", clientID)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, clientID, capturedID,
		"Should use valid client-supplied ID")
	assert.Equal(t, clientID, w.Header().Get("X-Request-ID"))
}

func TestRequestID_RejectsInvalidIDs(t *testing.T) {
	tests := []struct {
		name     string
		clientID string
		reason   string
	}{
		{
			name:     "unicode characters",
			clientID: "request-🔥-id",
			reason:   "Contains unicode emoji",
		},
		{
			name:     "newline injection",
			clientID: "request-id\ninjected-log-line",
			reason:   "Contains newline",
		},
		{
			name:     "special characters",
			clientID: "request<script>alert(1)</script>",
			reason:   "Contains HTML/script tags",
		},
		{
			name:     "oversized ID",
			clientID: strings.Repeat("a", 129),
			reason:   "Exceeds 128 character limit",
		},
		{
			name:     "whitespace",
			clientID: "request id with spaces",
			reason:   "Contains spaces",
		},
		{
			name:     "tab character",
			clientID: "request\tid",
			reason:   "Contains tab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(middleware.RequestID())

			var capturedID string
			router.GET("/test", func(c *gin.Context) {
				capturedID = middleware.GetRequestID(c)
				c.Status(200)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("X-Request-ID", tt.clientID)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.NotEqual(t, tt.clientID, capturedID,
				"Should reject invalid ID: %s", tt.reason)
			assert.NotEmpty(t, capturedID,
				"Should generate new UUID for invalid ID")
			assert.NotEqual(t, tt.clientID, w.Header().Get("X-Request-ID"),
				"Response header must not reflect invalid/injected client ID: %s", tt.reason)
			// Verify it looks like a UUID (contains hyphens, right length)
			assert.Contains(t, capturedID, "-",
				"Generated ID should be UUID format")
		})
	}
}

func TestRequestID_AcceptsValidFormats(t *testing.T) {
	tests := []struct {
		name     string
		clientID string
	}{
		{
			name:     "UUID format",
			clientID: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:     "alphanumeric with hyphens",
			clientID: "req-123-abc",
		},
		{
			name:     "dots and underscores",
			clientID: "request.id_123",
		},
		{
			name:     "mixed valid characters",
			clientID: "abc.123_def-456",
		},
		{
			name:     "single character",
			clientID: "a",
		},
		{
			name:     "max length (128 chars)",
			clientID: strings.Repeat("a", 128),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(middleware.RequestID())

			var capturedID string
			router.GET("/test", func(c *gin.Context) {
				capturedID = middleware.GetRequestID(c)
				c.Status(200)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("X-Request-ID", tt.clientID)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, tt.clientID, capturedID,
				"Should accept valid ID format")
			assert.Equal(t, tt.clientID, w.Header().Get("X-Request-ID"),
				"Response header should reflect accepted client ID")
		})
	}
}

func TestGetRequestID_ReturnsEmptyWhenNotSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	requestID := middleware.GetRequestID(c)
	assert.Empty(t, requestID, "Should return empty string when not set")
}

func TestGetRequestID_ReturnsEmptyWhenWrongType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("request_id", 12345) // Wrong type (int instead of string)

	requestID := middleware.GetRequestID(c)
	assert.Empty(t, requestID, "Should return empty string for wrong type")
}
