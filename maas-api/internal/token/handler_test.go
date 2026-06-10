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

func TestExtractUserInfo_TenantHeader(t *testing.T) {
	router := setupRouter(t)

	t.Run("ValidTenantHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, `["group1"]`)
		req.Header.Set(constant.HeaderTenant, "my-tenant")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var body token.UserContext
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "testuser", body.Username)
		assert.Equal(t, []string{"group1"}, body.Groups)
		assert.Equal(t, "my-tenant", body.Tenant)
	})

	t.Run("MissingTenantHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, `["group1"]`)
		// No tenant header set

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "004", body["refId"])
	})

	t.Run("WhitespaceOnlyTenantHeader", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(constant.HeaderUsername, "testuser")
		req.Header.Set(constant.HeaderGroup, `["group1"]`)
		req.Header.Set(constant.HeaderTenant, "   ")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "004", body["refId"])
	})
}
