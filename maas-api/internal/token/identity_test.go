package token_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const validIdentityEnvelope = `{
  "source": "oidc",
  "claims": {
    "iss": "https://issuer.example.com",
    "sub": "user-1",
    "aud": "maas-api",
    "azp": "maas-client",
    "name": "Alex User",
    "given_name": "Alex",
    "family_name": "User",
    "preferred_username": "alex",
    "email": "alex@example.com",
    "groups": ["42c3e46f-2477-44d7-a85b-d3b43f6b31fa/users"],
    "https://redhat.com/maas/orgs": [
      {
        "organization_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
        "organization_name": "coke",
        "roles": ["admins"]
      }
    ]
  }
}`

func TestParseIdentityEnvelope(t *testing.T) {
	identity, err := token.ParseIdentityEnvelope(validIdentityEnvelope)
	require.NoError(t, err)
	assert.Equal(t, "oidc", identity.Source)
	assert.Equal(t, "https://issuer.example.com", identity.Claims.Issuer)
	assert.Equal(t, "user-1", identity.Claims.Subject)
	assert.Equal(t, "maas-api", identity.Claims.Audience.Value)
	assert.Equal(t, "maas-client", identity.Claims.AuthorizedParty)
	assert.Equal(t, "Alex User", identity.Claims.Name)
	assert.Equal(t, "Alex", identity.Claims.GivenName)
	assert.Equal(t, "User", identity.Claims.FamilyName)
	assert.Equal(t, "alex", identity.Claims.PreferredUsername)
	assert.Equal(t, "alex@example.com", identity.Claims.Email)
	assert.Equal(t, []string{"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/users"}, identity.Claims.Groups)
	require.Len(t, identity.Claims.Organizations, 1)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", identity.Claims.Organizations[0].OrganizationID)
	assert.Equal(t, "coke", identity.Claims.Organizations[0].OrganizationName)
	assert.Equal(t, []string{"admins"}, identity.Claims.Organizations[0].Roles)
}

func TestParseIdentityEnvelope_ArrayAudience(t *testing.T) {
	envelope := strings.Replace(validIdentityEnvelope, `"aud": "maas-api"`, `"aud": ["other-api", "maas-api"]`, 1)
	identity, err := token.ParseIdentityEnvelope(envelope)
	require.NoError(t, err)
	assert.Equal(t, []string{"other-api", "maas-api"}, identity.Claims.Audience.Items)
}

func TestParseIdentityEnvelope_OptionalProfileClaimsAbsent(t *testing.T) {
	replacements := []string{
		`"name": "Alex User",`, "",
		`"given_name": "Alex",`, "",
		`"family_name": "User",`, "",
		`"preferred_username": "alex",`, "",
		`"email": "alex@example.com",`, "",
		`"organization_name": "coke",`, "",
	}
	envelope := strings.NewReplacer(replacements...).Replace(validIdentityEnvelope)

	identity, err := token.ParseIdentityEnvelope(envelope)
	require.NoError(t, err)
	assert.Empty(t, identity.Claims.Name)
	assert.Empty(t, identity.Claims.GivenName)
	assert.Empty(t, identity.Claims.FamilyName)
	assert.Empty(t, identity.Claims.PreferredUsername)
	assert.Empty(t, identity.Claims.Email)
	assert.Empty(t, identity.Claims.Organizations[0].OrganizationName)
}

func TestParseIdentityEnvelope_RejectsInvalidTransport(t *testing.T) {
	tests := []struct {
		name     string
		envelope string
	}{
		{name: "wrong source", envelope: strings.Replace(validIdentityEnvelope, `"oidc"`, `"authorino"`, 1)},
		{name: "missing claims", envelope: `{"source":"oidc"}`},
		{name: "unknown field", envelope: strings.Replace(validIdentityEnvelope, `"source": "oidc",`, `"source": "oidc", "extra": true,`, 1)},
		{name: "duplicate key", envelope: strings.Replace(validIdentityEnvelope, `"source": "oidc",`, `"source": "oidc", "source": "oidc",`, 1)},
		{name: "trailing data", envelope: validIdentityEnvelope + " {}"},
		{name: "bearer jwt", envelope: "header.payload.signature"},
		{name: "missing audience", envelope: strings.Replace(validIdentityEnvelope, `"aud": "maas-api",`, "", 1)},
		{name: "missing azp", envelope: strings.Replace(validIdentityEnvelope, `"azp": "maas-client",`, "", 1)},
		{name: "oversized profile claim", envelope: strings.Replace(validIdentityEnvelope, `"preferred_username": "alex"`, `"preferred_username": "`+strings.Repeat("a", 257)+`"`, 1)},
		{
			name: "oversized organization name",
			envelope: strings.Replace(
				validIdentityEnvelope,
				`"organization_name": "coke"`,
				`"organization_name": "`+strings.Repeat("a", 257)+`"`,
				1,
			),
		},
		{
			name: "noncanonical organization id",
			envelope: strings.Replace(
				validIdentityEnvelope,
				`"organization_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"`,
				`"organization_id": "42C3E46F-2477-44D7-A85B-D3B43F6B31FA"`,
				1,
			),
		},
		{name: "oversized", envelope: strings.Replace(validIdentityEnvelope, "https://issuer.example.com", strings.Repeat("a", 16*1024+1), 1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := token.ParseIdentityEnvelope(test.envelope)
			assert.Error(t, err)
		})
	}
}

func TestExtractVerifiedIdentityOptional(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")

	t.Run("valid envelope is stored", func(t *testing.T) {
		router := gin.New()
		router.Use(handler.ExtractVerifiedIdentityOptional())
		router.GET("/", func(c *gin.Context) {
			value, exists := c.Get("verified_identity")
			require.True(t, exists)
			identity, ok := value.(*token.VerifiedIdentity)
			require.True(t, ok)
			assert.Equal(t, "oidc", identity.Source)
			c.Status(http.StatusOK)
		})

		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusOK, recorder.Code)
	})

	t.Run("absent envelope preserves legacy request", func(t *testing.T) {
		router := gin.New()
		router.Use(handler.ExtractVerifiedIdentityOptional())
		router.GET("/", func(c *gin.Context) {
			_, exists := c.Get("verified_identity")
			assert.False(t, exists)
			c.Status(http.StatusOK)
		})

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		assert.Equal(t, http.StatusOK, recorder.Code)
	})

	t.Run("duplicate identity header is rejected", func(t *testing.T) {
		router := gin.New()
		router.Use(handler.ExtractVerifiedIdentityOptional())
		router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Add(constant.HeaderIdentity, validIdentityEnvelope)
		request.Header.Add(constant.HeaderIdentity, validIdentityEnvelope)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("empty identity header is rejected", func(t *testing.T) {
		router := gin.New()
		router.Use(handler.ExtractVerifiedIdentityOptional())
		router.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(constant.HeaderIdentity, "")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})
}

func TestGetMe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentity(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me?organization_id=another-org", nil)
	request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "https://issuer.example.com", response["iss"])
	assert.Equal(t, "user-1", response["sub"])
	assert.Equal(t, "Alex User", response["name"])
	assert.Equal(t, "Alex", response["given_name"])
	assert.Equal(t, "User", response["family_name"])
	assert.Equal(t, "alex", response["preferred_username"])
	assert.Equal(t, "alex@example.com", response["email"])
	assert.Equal(t, []any{"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/users"}, response["groups"])
	assert.Equal(t, []any{
		map[string]any{
			"organization_id":   "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
			"organization_name": "coke",
			"roles":             []any{"admins"},
		},
	}, response["https://redhat.com/maas/orgs"])
	assert.ElementsMatch(t, []string{
		"iss", "sub", "name", "given_name", "family_name", "preferred_username", "email", "groups",
		"https://redhat.com/maas/orgs",
	}, mapKeys(response))
	assert.NotContains(t, response, "is_admin")
	assert.NotContains(t, response, "subscriptions")
}

func TestGetMeRequiresVerifiedSSOIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentity(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderUsername, "alice")
	request.Header.Set(constant.HeaderGroup, `["team-a"]`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "alice")
	assert.NotContains(t, recorder.Body.String(), "team-a")
}

func TestGetMeAbsentOptionalMembershipSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentity(), handler.GetMe)

	envelope := `{
		"source": "oidc",
		"claims": {
			"iss": "https://issuer.example.com",
			"sub": "user-2",
			"aud": "maas-api",
			"azp": "maas-client"
		}
	}`
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, envelope)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var response token.IdentityResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "https://issuer.example.com", response.Issuer)
	assert.Equal(t, "user-2", response.Subject)
	assert.Empty(t, response.Name)
	assert.Empty(t, response.GivenName)
	assert.Empty(t, response.FamilyName)
	assert.Empty(t, response.PreferredUsername)
	assert.Empty(t, response.Email)
	assert.Empty(t, response.Groups)
	assert.Empty(t, response.Organizations)
}

func TestGetMeOmitsAbsentOrganizationRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentity(), handler.GetMe)

	envelope := `{
		"source": "oidc",
		"claims": {
			"iss": "https://issuer.example.com",
			"sub": "user-2",
			"aud": "maas-api",
			"azp": "maas-client",
			"https://redhat.com/maas/orgs": [
				{"organization_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"}
			]
		}
	}`
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, envelope)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	organizations, ok := response["https://redhat.com/maas/orgs"].([]any)
	require.True(t, ok)
	require.Len(t, organizations, 1)
	organization, ok := organizations[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", organization["organization_id"])
	assert.NotContains(t, organization, "roles")
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}
