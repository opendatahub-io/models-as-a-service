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
    "groups": ["users"],
    "org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
    "org_name": "coke",
    "roles": ["admins"]
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
	assert.Equal(t, []string{"users"}, identity.Claims.Groups)
	claims := identityClaimsMap(t, identity)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", claims["org_id"])
	assert.Equal(t, "coke", claims["org_name"])
	assert.Equal(t, []any{"admins"}, claims["roles"])
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
		`"org_name": "coke",`, "",
		`"roles": ["admins"]`, `"roles": []`,
	}
	envelope := strings.NewReplacer(replacements...).Replace(validIdentityEnvelope)

	identity, err := token.ParseIdentityEnvelope(envelope)
	require.NoError(t, err)
	assert.Empty(t, identity.Claims.Name)
	assert.Empty(t, identity.Claims.GivenName)
	assert.Empty(t, identity.Claims.FamilyName)
	assert.Empty(t, identity.Claims.PreferredUsername)
	assert.Empty(t, identity.Claims.Email)
	claims := identityClaimsMap(t, identity)
	assert.NotContains(t, claims, "org_name")
	assert.NotContains(t, claims, "roles")
}

func TestParseIdentityEnvelope_WithoutOrganizationIdentity(t *testing.T) {
	envelope := `{
		"source": "oidc",
		"claims": {
			"iss": "https://issuer.example.com",
			"sub": "user-1",
			"aud": "maas-api",
			"azp": "maas-client",
			"groups": ["users"]
		}
	}`

	identity, err := token.ParseIdentityEnvelope(envelope)
	require.NoError(t, err)
	assert.NotContains(t, identityClaimsMap(t, identity), "org_id")
}

func TestParseIdentityEnvelope_EmptyOrganizationGroups(t *testing.T) {
	envelope := strings.Replace(validIdentityEnvelope, `"groups": ["users"]`, `"groups": []`, 1)

	identity, err := token.ParseIdentityEnvelope(envelope)
	require.NoError(t, err)
	assert.Empty(t, identity.Claims.Groups)
}

func TestQualifiedGroupsUsesExportedOrganizationID(t *testing.T) {
	identity := &token.VerifiedIdentity{
		Source: "oidc",
		Claims: token.VerifiedClaims{
			OrgID:  "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
			Groups: []string{"users"},
		},
	}

	groups, err := token.QualifiedGroups(identity)
	require.NoError(t, err)
	assert.Equal(t, []string{"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/users"}, groups)
}

func TestParseIdentityEnvelope_RejectsInvalidTransport(t *testing.T) {
	nestedOrganizationClaim := strings.Replace(
		validIdentityEnvelope,
		`"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",`,
		`"https://redhat.com/maas/orgs": [],`,
		1,
	)
	invalidOrganizationID := strings.Replace(
		validIdentityEnvelope,
		`"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"`,
		`"org_id": "42C3E46F-2477-44D7-A85B-D3B43F6B31FA"`,
		1,
	)
	tooManyRoles := strings.Replace(
		validIdentityEnvelope,
		`"roles": ["admins"]`,
		`"roles": [`+strings.TrimSuffix(strings.Repeat(`"users",`, 65), ",")+`]`,
		1,
	)
	mixedCaseOrganizationID := strings.NewReplacer(
		`"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",`, `"Org_Id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"`,
		`"org_name": "coke",`, "",
		`"roles": ["admins"]`, "",
	).Replace(validIdentityEnvelope)

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
		{name: "removed nested organization claim", envelope: nestedOrganizationClaim},
		{name: "invalid organization uuid", envelope: invalidOrganizationID},
		{name: "null organization id", envelope: strings.Replace(validIdentityEnvelope, `"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"`, `"org_id": null`, 1)},
		{name: "wrong organization id type", envelope: strings.Replace(validIdentityEnvelope, `"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"`, `"org_id": 42`, 1)},
		{name: "null organization name", envelope: strings.Replace(validIdentityEnvelope, `"org_name": "coke"`, `"org_name": null`, 1)},
		{name: "wrong organization name type", envelope: strings.Replace(validIdentityEnvelope, `"org_name": "coke"`, `"org_name": false`, 1)},
		{name: "null roles", envelope: strings.Replace(validIdentityEnvelope, `"roles": ["admins"]`, `"roles": null`, 1)},
		{name: "wrong roles type", envelope: strings.Replace(validIdentityEnvelope, `"roles": ["admins"]`, `"roles": "admins"`, 1)},
		{name: "mixed-case roles key", envelope: strings.Replace(validIdentityEnvelope, `"roles": ["admins"]`, `"Roles": ["admins"]`, 1)},
		{name: "mixed-case organization id key", envelope: mixedCaseOrganizationID},
		{name: "empty organization group", envelope: strings.Replace(validIdentityEnvelope, `"groups": ["users"]`, `"groups": [""]`, 1)},
		{name: "duplicate organization group", envelope: strings.Replace(validIdentityEnvelope, `"groups": ["users"]`, `"groups": ["users", "users"]`, 1)},
		{name: "organization name without id", envelope: strings.Replace(validIdentityEnvelope, `"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",`, "", 1)},
		{name: "roles without id", envelope: strings.NewReplacer(
			`"org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",`, "",
			`"org_name": "coke",`, "",
		).Replace(validIdentityEnvelope)},
		{name: "empty organization role", envelope: strings.Replace(validIdentityEnvelope, `"roles": ["admins"]`, `"roles": [""]`, 1)},
		{name: "too many organization roles", envelope: tooManyRoles},
		{name: "oversized organization name", envelope: strings.Replace(validIdentityEnvelope, `"org_name": "coke"`, `"org_name": "`+strings.Repeat("a", 257)+`"`, 1)},
		{name: "too deeply nested", envelope: strings.Replace(validIdentityEnvelope, `"groups": ["users"]`, `"groups": [[[[[[["users"]]]]]]]`, 1)},
		{name: "oversized", envelope: strings.Replace(validIdentityEnvelope, "https://issuer.example.com", strings.Repeat("a", 16*1024+1), 1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := token.ParseIdentityEnvelope(test.envelope)
			assert.Error(t, err)
		})
	}
}

func identityClaimsMap(t *testing.T, identity *token.VerifiedIdentity) map[string]any {
	t.Helper()
	claimsJSON, err := json.Marshal(identity.Claims)
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(claimsJSON, &claims))
	return claims
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
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

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
	assert.Equal(t, []any{"users"}, response["groups"])
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", response["org_id"])
	assert.Equal(t, "coke", response["org_name"])
	assert.Equal(t, []any{"admins"}, response["roles"])
	assert.NotContains(t, response, "https://redhat.com/maas/orgs")
	assert.ElementsMatch(t, []string{
		"iss", "sub", "name", "given_name", "family_name", "preferred_username", "email", "groups",
		"org_id", "org_name", "roles",
	}, mapKeys(response))
	assert.NotContains(t, response, "is_admin")
	assert.NotContains(t, response, "subscriptions")
}

func TestGetMeRequiresAnAuthenticatedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderUsername, "alice")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "alice")
	assert.NotContains(t, recorder.Body.String(), "team-a")
}

func TestGetMeUsesGatewayIdentityForOtherAuthenticationMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderUsername, "alice")
	request.Header.Set(constant.HeaderGroup, `["team-a","team-b"]`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "alice", response["sub"])
	assert.Equal(t, "alice", response["name"])
	assert.Equal(t, "alice", response["preferred_username"])
	assert.Equal(t, []any{"team-a", "team-b"}, response["groups"])
	assert.NotContains(t, response, "iss")
	assert.NotContains(t, response, "org_id")
	assert.NotContains(t, response, "org_name")
	assert.NotContains(t, response, "roles")
	assert.NotContains(t, response, "https://redhat.com/maas/orgs")
}

func TestGetMeRejectsInferenceAPIKeys(t *testing.T) {
	tests := []struct {
		name   string
		header func(http.Header)
	}{
		{
			name: "bearer",
			header: func(header http.Header) {
				header.Set("Authorization", "Bearer sk-oai-key-id-secret")
			},
		},
		{
			name: "x-api-key",
			header: func(header http.Header) {
				header.Set("X-Api-Key", "sk-oai-key-id-secret")
			},
		},
		{
			name: "repeated authorization",
			header: func(header http.Header) {
				header.Set("Authorization", "Bearer user-token")
				header.Add("Authorization", "Bearer sk-oai-key-id-secret")
			},
		},
		{
			name: "repeated x-api-key",
			header: func(header http.Header) {
				header.Set("X-Api-Key", "user-token")
				header.Add("X-Api-Key", "sk-oai-key-id-secret")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			handler := token.NewHandler(logger.New(false), "tenant")
			router := gin.New()
			router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

			request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			request.Header.Set(constant.HeaderUsername, "alice")
			request.Header.Set(constant.HeaderGroup, `["team-a"]`)
			test.header(request.Header)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			assert.NotContains(t, recorder.Body.String(), "alice")
		})
	}
}

func TestGetMeRejectsInferenceAPIKeysWithVerifiedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
	request.Header.Set("X-Api-Key", "sk-oai-key-id-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "user-1")
	assert.NotContains(t, recorder.Body.String(), "alex")
}

func TestGetMePrefersVerifiedSSOIdentityOverGatewayIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
	request.Header.Set(constant.HeaderUsername, "gateway-user")
	request.Header.Set(constant.HeaderGroup, `["gateway-group"]`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var response token.IdentityResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "https://issuer.example.com", response.Issuer)
	assert.Equal(t, "user-1", response.Subject)
	assert.Equal(t, "alex", response.PreferredUsername)
	assert.Equal(t, []string{"users"}, response.Groups)
}

func TestGetMeIgnoresIncompleteLegacyHeadersWhenVerifiedIdentityPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
	request.Header.Set(constant.HeaderGroup, `["gateway-group"]`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var response token.IdentityResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "https://issuer.example.com", response.Issuer)
	assert.Equal(t, "user-1", response.Subject)
}

func TestGetMeIgnoresMalformedLegacyHeadersWhenVerifiedIdentityPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentityOptional(), handler.ExtractUserInfoOptional(), handler.GetMe)

	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, validIdentityEnvelope)
	request.Header.Set(constant.HeaderUsername, "gateway-user")
	request.Header.Set(constant.HeaderGroup, "not-valid-format")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var response token.IdentityResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "https://issuer.example.com", response.Issuer)
	assert.Equal(t, "user-1", response.Subject)
}

func TestGetMeWithoutOrganizationIdentitySucceeds(t *testing.T) {
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
	responseJSON, err := json.Marshal(response)
	require.NoError(t, err)
	var responseFields map[string]any
	require.NoError(t, json.Unmarshal(responseJSON, &responseFields))
	assert.NotContains(t, responseFields, "org_id")
	assert.NotContains(t, responseFields, "org_name")
	assert.NotContains(t, responseFields, "roles")
	assert.NotContains(t, responseFields, "https://redhat.com/maas/orgs")
}

func TestGetMeOmitsAbsentOrganizationNameAndEmptyRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := token.NewHandler(logger.New(false), "tenant")
	router := gin.New()
	router.GET("/v1/me", handler.ExtractVerifiedIdentity(), handler.GetMe)

	envelope := strings.NewReplacer(
		`"org_name": "coke",`, "",
		`"roles": ["admins"]`, `"roles": []`,
	).Replace(validIdentityEnvelope)
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set(constant.HeaderIdentity, envelope)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)

	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", response["org_id"])
	assert.NotContains(t, response, "org_name")
	assert.NotContains(t, response, "roles")
	assert.NotContains(t, response, "https://redhat.com/maas/orgs")
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}
