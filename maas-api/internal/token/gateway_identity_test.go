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

const testGatewayToken = "gateway-test-secret-token"

func setupGatewayProtectedRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := token.NewHandler(logger.Development(), "test")
	router := gin.New()
	router.Use(
		token.RequireGatewayIdentity(logger.Development(), testGatewayToken),
		h.ExtractUserInfo(),
	)
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

func TestRequireGatewayIdentity_RejectsForgedIdentityWithoutGatewayToken(t *testing.T) {
	router := setupGatewayProtectedRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(constant.HeaderUsername, "hacked-user")
	req.Header.Set(constant.HeaderGroup, `["system:cluster-admins"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireGatewayIdentity_RejectsWrongGatewayToken(t *testing.T) {
	router := setupGatewayProtectedRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(constant.HeaderGatewayAuth, "wrong-token")
	req.Header.Set(constant.HeaderUsername, "alice")
	req.Header.Set(constant.HeaderGroup, `["system:authenticated"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireGatewayIdentity_AllowsGatewayAuthenticatedRequest(t *testing.T) {
	router := setupGatewayProtectedRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(constant.HeaderGatewayAuth, testGatewayToken)
	req.Header.Set(constant.HeaderUsername, "alice")
	req.Header.Set(constant.HeaderGroup, `["system:authenticated"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var body token.UserContext
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "alice", body.Username)
	assert.Equal(t, []string{"system:authenticated"}, body.Groups)
}

func TestRequireGatewayIdentity_SkipsVerificationWhenTokenNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := token.NewHandler(logger.Development(), "test")
	router := gin.New()
	router.Use(
		token.RequireGatewayIdentity(logger.Development(), ""),
		h.ExtractUserInfo(),
	)
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(constant.HeaderUsername, "alice")
	req.Header.Set(constant.HeaderGroup, `["system:authenticated"]`)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireGatewayIdentityIfClaims_AllowsMissingIdentityWithoutToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(token.RequireGatewayIdentityIfClaims(logger.Development(), testGatewayToken))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireGatewayIdentityIfClaims_RejectsForgedIdentityWithoutGatewayToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(token.RequireGatewayIdentityIfClaims(logger.Development(), testGatewayToken))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(constant.HeaderUsername, "hacked-user")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
