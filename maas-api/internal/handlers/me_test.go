package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestGetMeReturnsVerifiedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	identity := &token.VerifiedIdentity{
		Source: "oidc",
		Claims: token.VerifiedClaims{
			Subject: "user-1",
			Email:   "alex@example.com",
			Name:    "Alex Doe",
			Organizations: []token.OrganizationMembership{
				{
					OrganizationID:   "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
					OrganizationName: "coke",
					Roles:            []string{"admins"},
				},
			},
		},
	}

	router := gin.New()
	router.GET("/v1/me", func(c *gin.Context) {
		c.Set("verified_identity", identity)
		c.Next()
	}, handlers.NewIdentityHandler(logger.New(false)).GetMe)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "user-1", response["sub"])
	assert.Equal(t, "alex@example.com", response["email"])
	assert.Equal(t, "Alex Doe", response["name"])

	organizations := response["https://redhat.com/maas/orgs"].([]any)
	require.Len(t, organizations, 1)
	organization := organizations[0].(map[string]any)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", organization["organization_id"])
	assert.Equal(t, "coke", organization["organization_name"])
	assert.Equal(t, []any{"admins"}, organization["roles"])
}

func TestGetMeRequiresVerifiedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/v1/me", handlers.NewIdentityHandler(logger.New(false)).GetMe)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestGetMeRejectsLegacyIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/v1/me", func(c *gin.Context) {
		c.Set("user", &token.UserContext{
			Username: "legacy-user",
			Groups:   []string{"system:authenticated"},
			Tenant:   "grid-tenant",
		})
		c.Next()
	}, handlers.NewIdentityHandler(logger.New(false)).GetMe)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}
