package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v2/packages/pagination"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"knative.dev/pkg/apis"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

func TestListingModels(t *testing.T) {
	testLogger := logger.Development()
	strptr := func(s string) *string { return &s }

	// Create mock HTTP server to simulate authorization responses
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept any Bearer token - the token exchange produces JWT tokens
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") && len(authHeader) > 7 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer authServer.Close()

	const (
		testGatewayName      = "test-gateway"
		testGatewayNamespace = "test-gateway-ns"
	)

	llmTestScenarios := []fixtures.LLMTestScenario{
		{
			Name:             "llama-7b",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				assert.Nil(t, model.Details, "Expected modelDetails to be nil for model without annotations")
			},
		},
		{
			Name:             "gpt-3-turbo",
			Namespace:        "openai-models",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "bert-base",
			Namespace:        "nlp-models",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            false,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "llama-7b-private-url",
			Namespace:        "model-serving",
			URL:              fixtures.AddressEntry(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "model-without-url",
			Namespace:        fixtures.TestNamespace,
			URL:              fixtures.PublicURL(""),
			Ready:            false,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "fallback-model-name",
			Namespace:        fixtures.TestNamespace,
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			SpecModelName:    strptr("fallback-model-name"),
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "model-with-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			Annotations: map[string]string{
				constant.AnnotationGenAIUseCase: "General purpose LLM",
				constant.AnnotationDescription:  "A large language model for general AI tasks",
				constant.AnnotationDisplayName:  "Test Model Alpha",
			},
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				require.NotNil(t, model.Details, "Expected modelDetails to be present")
				assert.Equal(t, "General purpose LLM", model.Details.GenAIUseCase)
				assert.Equal(t, "A large language model for general AI tasks", model.Details.Description)
				assert.Equal(t, "Test Model Alpha", model.Details.DisplayName)
			},
		},
		{
			Name:             "model-with-partial-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			Annotations: map[string]string{
				constant.AnnotationDisplayName: "Test Model Beta",
			},
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				require.NotNil(t, model.Details, "Expected modelDetails to be present")
				assert.Empty(t, model.Details.GenAIUseCase)
				assert.Empty(t, model.Details.Description)
				assert.Equal(t, "Test Model Beta", model.Details.DisplayName)
			},
		},
		{
			Name:             "model-with-empty-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(authServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			Annotations: map[string]string{
				constant.AnnotationDisplayName: "",
			},
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				assert.Nil(t, model.Details, "Expected modelDetails to be nil when annotation values are empty strings")
			},
		},
	}
	llmInferenceServices := fixtures.CreateLLMInferenceServices(llmTestScenarios...)

	config := fixtures.TestServerConfig{
		Objects: llmInferenceServices,
	}
	router, clients := fixtures.SetupTestServer(t, config)

	gatewayRef := models.GatewayRef{
		Name:      testGatewayName,
		Namespace: testGatewayNamespace,
	}

	modelMgr, errMgr := models.NewManager(
		testLogger,
		clients.InferenceServiceLister,
		clients.LLMInferenceServiceLister,
		clients.HTTPRouteLister,
		gatewayRef,
	)
	require.NoError(t, errMgr)

	// Create token manager for test - uses fixtures helper which sets up tier config
	tokenManager, _, cleanup := fixtures.StubTokenProviderAPIs(t, true)
	defer cleanup()

	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, tokenManager)

	// Create token handler to extract user info middleware
	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant, tokenManager)

	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	w := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
	require.NoError(t, err, "Failed to create request")

	// Set headers required by ExtractUserInfo middleware
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set(constant.HeaderUsername, "test-user@example.com")
	req.Header.Set(constant.HeaderGroup, `["free-users"]`)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Expected status OK")

	var response pagination.Page[models.Model]
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err, "Failed to unmarshal response body")

	assert.Equal(t, "list", response.Object, "Expected object type to be 'list'")
	// With authorization, we expect 8 models (excluding the one without URL)
	require.Len(t, response.Data, len(llmInferenceServices)-1, "Mismatched number of models returned")

	modelsByName := make(map[string]models.Model)
	for _, model := range response.Data {
		modelsByName[model.ID] = model
	}

	for _, scenario := range llmTestScenarios {
		// Skip the model without URL as it should be filtered out by authorization
		if scenario.Name == "model-without-url" {
			continue
		}

		// expected ID mirrors toModels(): fallback to metadata.name unless spec.model.name is non-empty
		expectedModelID := scenario.Name
		if scenario.SpecModelName != nil && *scenario.SpecModelName != "" {
			expectedModelID = *scenario.SpecModelName
		}

		t.Run(expectedModelID, func(t *testing.T) {
			actualModel, exists := modelsByName[expectedModelID]
			require.True(t, exists, "Model '%s' not found in response", expectedModelID)

			assert.NotZero(t, actualModel.Created, "Expected 'Created' timestamp to be set")

			assert.Equal(t, expectedModelID, actualModel.ID)
			assert.Equal(t, "model", string(actualModel.Object))
			assert.Equal(t, mustParseURL(scenario.URL.String()), actualModel.URL)
			assert.Equal(t, scenario.Ready, actualModel.Ready)

			// Run scenario-specific assertions if defined
			if scenario.AssertDetails != nil {
				scenario.AssertDetails(t, actualModel)
			}
		})
	}
}

func mustParseURL(rawURL string) *apis.URL {
	if rawURL == "" {
		return nil
	}
	u, err := apis.ParseURL(rawURL)
	if err != nil {
		panic("test setup failed: invalid URL: " + err.Error())
	}
	return u
}
