package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v2/packages/pagination"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"knative.dev/pkg/apis"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

const (
	maasModelGVRGroup    = "maas.opendatahub.io"
	maasModelGVRVersion  = "v1alpha1"
	maasModelGVRResource = "maasmodels"
)

// maasModelUnstructured returns an unstructured MaaSModel for testing (name, namespace, endpoint URL, ready).
func maasModelUnstructured(name, namespace, endpoint string, ready bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   maasModelGVRGroup,
		Version: maasModelGVRVersion,
		Kind:    "MaaSModel",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	u.SetCreationTimestamp(metav1.NewTime(time.Unix(1700000000, 0)))
	_ = unstructured.SetNestedField(u.Object, endpoint, "status", "endpoint")
	if ready {
		_ = unstructured.SetNestedField(u.Object, "Ready", "status", "phase")
	}
	_ = unstructured.SetNestedField(u.Object, "llmisvc", "spec", "modelRef", "kind")
	return u
}

// fakeMaaSModelLister implements models.MaaSModelLister for tests (namespace -> items).
type fakeMaaSModelLister map[string][]*unstructured.Unstructured

func (f fakeMaaSModelLister) List(namespace string) ([]*unstructured.Unstructured, error) {
	items := f[namespace]
	if items == nil {
		return nil, nil
	}
	out := make([]*unstructured.Unstructured, len(items))
	for i, u := range items {
		out[i] = u.DeepCopy()
	}
	return out, nil
}

// fakeSubscriptionLister implements subscription.Lister for tests.
// Returns a single default subscription so that tests auto-select it.
type fakeSubscriptionLister struct{}

func (f *fakeSubscriptionLister) List() ([]*unstructured.Unstructured, error) {
	// Return a single subscription that matches all users
	sub := &unstructured.Unstructured{}
	sub.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "maas.opendatahub.io",
		Version: "v1alpha1",
		Kind:    "MaaSSubscription",
	})
	sub.SetName("test-subscription")
	sub.SetNamespace(fixtures.TestNamespace)

	// Set spec.owner.groups to match test users
	_ = unstructured.SetNestedSlice(sub.Object, []any{
		map[string]any{"name": "free-users"},
		map[string]any{"name": "premium-users"},
	}, "spec", "owner", "groups")

	return []*unstructured.Unstructured{sub}, nil
}

// fakeMultiSubscriptionLister returns multiple subscriptions by name -> groups mapping.
type fakeMultiSubscriptionLister map[string][]string

func (f fakeMultiSubscriptionLister) List() ([]*unstructured.Unstructured, error) {
	result := make([]*unstructured.Unstructured, 0, len(f))
	for subName, groups := range f {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(subName)
		sub.SetNamespace(fixtures.TestNamespace)

		// Set spec.owner.groups
		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}
		_ = unstructured.SetNestedSlice(sub.Object, groupSlice, "spec", "owner", "groups")

		result = append(result, sub)
	}
	return result, nil
}

// makeModelsResponse creates a JSON response for /v1/models endpoint.
func makeModelsResponse(modelIDs ...string) []byte {
	if len(modelIDs) == 0 {
		return []byte(`{"object":"list","data":[]}`)
	}
	if len(modelIDs) == 1 {
		return fmt.Appendf(nil, `{"object":"list","data":[{"id":%q,"object":"model","created":1700000000,"owned_by":"test"}]}`, modelIDs[0])
	}
	// Multiple models
	data := make([]string, 0, len(modelIDs))
	for _, id := range modelIDs {
		data = append(data, fmt.Sprintf(`{"id":%q,"object":"model","created":1700000000,"owned_by":"test"}`, id))
	}
	return fmt.Appendf(nil, `{"object":"list","data":[%s]}`, strings.Join(data, ","))
}

// createMockModelServer creates a test server that returns a valid /v1/models response.
func createMockModelServer(t *testing.T, modelID string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(authHeader, "Bearer ") && len(authHeader) > 7 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(makeModelsResponse(modelID))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// createMockModelServerWithSubscriptionCheck creates a test server that checks both Authorization and x-maas-subscription headers.
func createMockModelServerWithSubscriptionCheck(t *testing.T, modelID string, requiredSubscription string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		subscriptionHeader := r.Header.Get("X-Maas-Subscription")
		w.Header().Set("Content-Type", "application/json")

		// Check authorization
		if !strings.HasPrefix(authHeader, "Bearer ") || len(authHeader) <= 7 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Check subscription (if required)
		if requiredSubscription != "" && subscriptionHeader != requiredSubscription {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse(modelID))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestListingModels(t *testing.T) {
	testLogger := logger.Development()
	strptr := func(s string) *string { return &s }

	const (
		testGatewayName      = "test-gateway"
		testGatewayNamespace = "test-gateway-ns"
	)

	// Create individual mock servers for each model that return valid /v1/models responses
	llamaServer := createMockModelServer(t, "llama-7b")
	gptServer := createMockModelServer(t, "gpt-3-turbo")
	bertServer := createMockModelServer(t, "bert-base")
	llamaPrivateServer := createMockModelServer(t, "llama-7b-private-url")
	fallbackServer := createMockModelServer(t, "fallback-model-name")
	metadataServer := createMockModelServer(t, "model-with-metadata")
	partialMetadataServer := createMockModelServer(t, "model-with-partial-metadata")
	emptyMetadataServer := createMockModelServer(t, "model-with-empty-metadata")

	llmTestScenarios := []fixtures.LLMTestScenario{
		{
			Name:             "llama-7b",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(llamaServer.URL),
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
			URL:              fixtures.PublicURL(gptServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "bert-base",
			Namespace:        "nlp-models",
			URL:              fixtures.PublicURL(bertServer.URL),
			Ready:            false,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "llama-7b-private-url",
			Namespace:        "model-serving",
			URL:              fixtures.AddressEntry(llamaPrivateServer.URL),
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
			URL:              fixtures.PublicURL(fallbackServer.URL),
			Ready:            true,
			SpecModelName:    strptr("fallback-model-name"),
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
		},
		{
			Name:             "model-with-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(metadataServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			Annotations: map[string]string{
				constant.AnnotationGenAIUseCase: "General purpose LLM",
				constant.AnnotationDescription:  "A large language model for general AI tasks",
				constant.AnnotationDisplayName:  "Test Model Alpha",
			},
			// MaaSModel listing does not populate Details from annotations.
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				_ = model
			},
		},
		{
			Name:             "model-with-partial-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(partialMetadataServer.URL),
			Ready:            true,
			GatewayName:      testGatewayName,
			GatewayNamespace: testGatewayNamespace,
			Annotations: map[string]string{
				constant.AnnotationDisplayName: "Test Model Beta",
			},
			// MaaSModel listing does not populate Details.
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				_ = model
			},
		},
		{
			Name:             "model-with-empty-metadata",
			Namespace:        "model-serving",
			URL:              fixtures.PublicURL(emptyMetadataServer.URL),
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
	// Build MaaSModel unstructured list from scenarios (same URLs as mock servers for access validation).
	maasModelItems := make([]*unstructured.Unstructured, 0, len(llmTestScenarios))
	for _, s := range llmTestScenarios {
		endpoint := s.URL.String()
		maasModelItems = append(maasModelItems, maasModelUnstructured(s.Name, fixtures.TestNamespace, endpoint, s.Ready))
	}
	maasModelLister := fakeMaaSModelLister{fixtures.TestNamespace: maasModelItems}

	config := fixtures.TestServerConfig{
		Objects: []runtime.Object{},
	}
	router, _ := fixtures.SetupTestServer(t, config)

	modelMgr, errMgr := models.NewManager(testLogger)
	require.NoError(t, errMgr)

	// Create token manager for test - uses fixtures helper which sets up tier config
	tokenManager, _, cleanup := fixtures.StubTokenProviderAPIs(t, true)
	defer cleanup()

	// Create a mock subscription selector that auto-selects for single subscription users
	subscriptionSelector := subscription.NewSelector(testLogger, &fakeSubscriptionLister{})

	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, tokenManager, subscriptionSelector, maasModelLister, fixtures.TestNamespace)

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
	require.Len(t, response.Data, len(llmTestScenarios)-1, "Mismatched number of models returned")

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

func TestListingModelsWithSubscriptionHeader(t *testing.T) {
	testLogger := logger.Development()

	// Create mock servers that require specific subscription headers
	premiumModelServer := createMockModelServerWithSubscriptionCheck(t, "premium-model", "premium")
	freeModelServer := createMockModelServerWithSubscriptionCheck(t, "free-model", "free")

	// Build MaaSModel unstructured list
	maasModelItems := []*unstructured.Unstructured{
		maasModelUnstructured("premium-model", fixtures.TestNamespace, premiumModelServer.URL, true),
		maasModelUnstructured("free-model", fixtures.TestNamespace, freeModelServer.URL, true),
	}
	maasModelLister := fakeMaaSModelLister{fixtures.TestNamespace: maasModelItems}

	config := fixtures.TestServerConfig{
		Objects: []runtime.Object{},
	}
	router, _ := fixtures.SetupTestServer(t, config)

	modelMgr, errMgr := models.NewManager(testLogger)
	require.NoError(t, errMgr)

	tokenManager, _, cleanup := fixtures.StubTokenProviderAPIs(t, true)
	defer cleanup()

	// Create subscription lister with premium and free subscriptions
	multiSubLister := &fakeMultiSubscriptionLister{
		"premium": []string{"premium-users"},
		"free":    []string{"free-users"},
	}
	subscriptionSelector := subscription.NewSelector(testLogger, multiSubLister)

	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, tokenManager, subscriptionSelector, maasModelLister, fixtures.TestNamespace)
	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant, tokenManager)

	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	// Table-driven tests for subscription header variants
	subscriptionTests := []struct {
		name               string
		subscription       string
		userGroups         string
		expectedModelID    string
		expectedModelCount int
	}{
		{
			name:               "with premium subscription header",
			subscription:       "premium",
			userGroups:         `["premium-users"]`,
			expectedModelID:    "premium-model",
			expectedModelCount: 1,
		},
		{
			name:               "with free subscription header",
			subscription:       "free",
			userGroups:         `["free-users"]`,
			expectedModelID:    "free-model",
			expectedModelCount: 1,
		},
	}

	for _, tt := range subscriptionTests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
			require.NoError(t, err, "Failed to create request")

			req.Header.Set("Authorization", "Bearer valid-token")
			req.Header.Set("X-Maas-Subscription", tt.subscription)
			req.Header.Set(constant.HeaderUsername, "test-user@example.com")
			req.Header.Set(constant.HeaderGroup, tt.userGroups)
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code, "Expected status OK")

			var response pagination.Page[models.Model]
			err = json.Unmarshal(w.Body.Bytes(), &response)
			require.NoError(t, err, "Failed to unmarshal response body")

			require.Len(t, response.Data, tt.expectedModelCount, "Expected model count mismatch")
			assert.Equal(t, tt.expectedModelID, response.Data[0].ID)
		})
	}

	t.Run("without subscription header - single subscription auto-selects", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err, "Failed to create request")

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["free-users"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "Expected status OK")

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err, "Failed to unmarshal response body")

		// User only has access to "free" subscription, so it auto-selects and returns free model
		require.Len(t, response.Data, 1, "Expected one model with auto-selected free subscription")
		assert.Equal(t, "free-model", response.Data[0].ID)
	})

	t.Run("without subscription header - multiple subscriptions requires header", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err, "Failed to create request")

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["free-users", "premium-users"]`)
		router.ServeHTTP(w, req)

		// User has access to both subscriptions, must specify which one
		// Returns 403 for consistency with inferencing (Authorino limitation)
		require.Equal(t, http.StatusForbidden, w.Code, "Expected 403 Forbidden")

		var errorResponse map[string]any
		err = json.Unmarshal(w.Body.Bytes(), &errorResponse)
		require.NoError(t, err, "Failed to unmarshal error response")

		errorObj, ok := errorResponse["error"].(map[string]any)
		require.True(t, ok, "Expected error object")
		assert.Equal(t, "permission_error", errorObj["type"])
	})
}
