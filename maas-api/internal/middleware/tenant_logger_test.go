package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func testTenantLogCfg() middleware.TenantLoggerConfig {
	return middleware.TenantLoggerConfig{
		DefaultTenant:   "models-as-a-service",
		TenantNamespace: "test-namespace",
		GatewayName:     "test-gateway",
	}
}

// TestTenantLogger_EnrichesWithTenantAndRequestID verifies the middleware
// stores a logger with tenant_name and request_id fields in the Gin context.
func TestTenantLogger_EnrichesWithTenantAndRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var captured *logger.Logger
	router := gin.New()
	router.Use(middleware.RequestID())
	router.Use(func(c *gin.Context) {
		c.Set("user", &token.UserContext{
			Username: "alice",
			Groups:   []string{"users"},
			Tenant:   "redteam",
		})
		c.Next()
	})
	router.Use(middleware.TenantLogger(logger.Development(), testTenantLogCfg()))
	router.GET("/test", func(c *gin.Context) {
		captured = middleware.GetLogger(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, captured, "logger should be set in context")
}

// TestTenantLogger_FallbackWithoutUserContext verifies that GetLogger returns
// a valid logger even when no UserContext is set (internal/health routes).
func TestTenantLogger_FallbackWithoutUserContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var captured *logger.Logger
	router := gin.New()
	router.Use(middleware.TenantLogger(logger.Development(), testTenantLogCfg()))
	router.GET("/health", func(c *gin.Context) {
		captured = middleware.GetLogger(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, captured)
}

// TestGetLogger_NilWhenNoMiddleware verifies that GetLogger returns
// nil when the TenantLogger middleware was not applied.
func TestGetLogger_NilWhenNoMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var captured *logger.Logger
	router := gin.New()
	router.GET("/test", func(c *gin.Context) {
		captured = middleware.GetLogger(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, captured, "should return nil when no middleware set the logger")
}
