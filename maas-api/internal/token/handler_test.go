package token_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := token.NewHandler(logger.Development(), "test")
	router := gin.New()
	router.Use(h.ExtractUserInfo())
	router.GET("/test", func(c *gin.Context) {
		user, exists := c.Get("user")
		if !exists {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "user not found in context"})
			return
		}
		c.JSON(http.StatusOK, user)
	})

	return router
}

// TestExtractUserInfo_TenantFromConfig verifies that the ExtractUserInfo middleware
// correctly sets the tenant from the handler's configured tenantName (from TENANT_NAME env var).
func TestExtractUserInfo_TenantFromConfig(t *testing.T) {
	router := setupRouter(t)

	t.Run("TenantFromHandler", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, `["group1"]`)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var body token.UserContext
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "testuser", body.Username)
		assert.Equal(t, []string{"group1"}, body.Groups)
		assert.Equal(t, "test", body.Tenant, "tenant should come from handler config")
	})
}

// setupOptionalRouter creates a test router using ExtractUserInfoOptional middleware.
func setupOptionalRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := token.NewHandler(logger.Development(), "test")
	router := gin.New()
	router.Use(h.ExtractUserInfoOptional())
	router.GET("/test", func(c *gin.Context) {
		user, exists := c.Get("user")
		if !exists {
			c.JSON(http.StatusOK, gin.H{"user_present": false})
			return
		}
		uc, ok := user.(*token.UserContext)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid user context"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"user_present": true,
			"username":     uc.Username,
			"groups":       uc.Groups,
			"tenant":       uc.Tenant,
		})
	})

	return router
}

func TestExtractUserInfoOptional(t *testing.T) {
	t.Run("sets user context when both headers present", func(t *testing.T) {
		router := setupOptionalRouter(t)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, `["group1"]`)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, true, body["user_present"])
		assert.Equal(t, "testuser", body["username"])
		assert.Equal(t, "test", body["tenant"])
	})

	t.Run("continues without user context when no headers present", func(t *testing.T) {
		router := setupOptionalRouter(t)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		// No auth headers set

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, false, body["user_present"])
	})

	t.Run("aborts when only username header present", func(t *testing.T) {
		router := setupOptionalRouter(t)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		// No group header

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "AUTH_FAILURE", body["exceptionCode"])
	})

	t.Run("aborts when only group header present", func(t *testing.T) {
		router := setupOptionalRouter(t)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderGroup, `["group1"]`)
		// No username header

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "AUTH_FAILURE", body["exceptionCode"])
	})

	t.Run("aborts when group header is malformed", func(t *testing.T) {
		router := setupOptionalRouter(t)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, "not-valid-format")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "AUTH_FAILURE", body["exceptionCode"])
	})
}
