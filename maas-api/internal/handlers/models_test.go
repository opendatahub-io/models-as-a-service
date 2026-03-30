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
	maasModelRefGVRGroup    = "maas.opendatahub.io"
	maasModelRefGVRVersion  = "v1alpha1"
	maasModelRefGVRResource = "maasmodelrefs"
)

// maasModelRefUnstructured returns an unstructured MaaSModelRef for testing (name, namespace, endpoint URL, ready, annotations).
func maasModelRefUnstructured(name, namespace, endpoint string, ready bool, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   maasModelRefGVRGroup,
		Version: maasModelRefGVRVersion,
		Kind:    "MaaSModelRef",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	u.SetCreationTimestamp(metav1.NewTime(time.Unix(1700000000, 0)))
	_ = unstructured.SetNestedField(u.Object, endpoint, "status", "endpoint")
	if ready {
		_ = unstructured.SetNestedField(u.Object, "Ready", "status", "phase")
	}
	_ = unstructured.SetNestedField(u.Object, "llmisvc", "spec", "modelRef", "kind")
	if len(annotations) > 0 {
		u.SetAnnotations(annotations)
	}
	return u
}

// fakeMaaSModelRefLister implements models.MaaSModelRefLister for tests (namespace -> items).
type fakeMaaSModelRefLister map[string][]*unstructured.Unstructured

func (f fakeMaaSModelRefLister) List() ([]*unstructured.Unstructured, error) {
	// Return all items from all namespaces
	var out []*unstructured.Unstructured
	for _, items := range f {
		for _, u := range items {
			out = append(out, u.DeepCopy())
		}
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
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				require.NotNil(t, model.Details, "Expected modelDetails to be populated from annotations")
				assert.Equal(t, "Test Model Alpha", model.Details.DisplayName)
				assert.Equal(t, "A large language model for general AI tasks", model.Details.Description)
				assert.Equal(t, "General purpose LLM", model.Details.GenAIUseCase)
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
			AssertDetails: func(t *testing.T, model models.Model) {
				t.Helper()
				require.NotNil(t, model.Details, "Expected modelDetails to be populated from annotations")
				assert.Equal(t, "Test Model Beta", model.Details.DisplayName)
				assert.Empty(t, model.Details.Description)
				assert.Empty(t, model.Details.GenAIUseCase)
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
	// Build MaaSModelRef unstructured list from scenarios (same URLs as mock servers for access validation).
	maasModelRefItems := make([]*unstructured.Unstructured, 0, len(llmTestScenarios))
	for _, s := range llmTestScenarios {
		endpoint := s.URL.String()
		maasModelRefItems = append(maasModelRefItems, maasModelRefUnstructured(s.Name, fixtures.TestNamespace, endpoint, s.Ready, s.Annotations))
	}
	maasModelRefLister := fakeMaaSModelRefLister{fixtures.TestNamespace: maasModelRefItems}

	config := fixtures.TestServerConfig{
		Objects: []runtime.Object{},
	}
	router, _ := fixtures.SetupTestServer(t, config)

	modelMgr, errMgr := models.NewManager(testLogger)
	require.NoError(t, errMgr)

	// Set up test fixtures
	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	// Create a mock subscription selector that auto-selects for single subscription users
	subscriptionSelector := subscription.NewSelector(testLogger, &fakeSubscriptionLister{})

	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, maasModelRefLister)

	// Create token handler to extract user info middleware
	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)

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
	// Use bare subscription names to match what Authorino injects from API key validation
	premiumModelServer := createMockModelServerWithSubscriptionCheck(t, "premium-model", "premium")
	freeModelServer := createMockModelServerWithSubscriptionCheck(t, "free-model", "free")

	// Build MaaSModelRef unstructured list
	maasModelRefItems := []*unstructured.Unstructured{
		maasModelRefUnstructured("premium-model", fixtures.TestNamespace, premiumModelServer.URL, true, nil),
		maasModelRefUnstructured("free-model", fixtures.TestNamespace, freeModelServer.URL, true, nil),
	}
	maasModelRefLister := fakeMaaSModelRefLister{fixtures.TestNamespace: maasModelRefItems}

	config := fixtures.TestServerConfig{
		Objects: []runtime.Object{},
	}
	router, _ := fixtures.SetupTestServer(t, config)

	modelMgr, errMgr := models.NewManager(testLogger)
	require.NoError(t, errMgr)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	// Create subscription lister with premium and free subscriptions
	multiSubLister := fakeMultiSubscriptionLister{
		"premium": []string{"premium-users"},
		"free":    []string{"free-users"},
	}
	subscriptionSelector := subscription.NewSelector(testLogger, multiSubLister)

	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, maasModelRefLister)
	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)

	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	// Table-driven tests for API key auth (X-MaaS-Subscription header injected by Authorino)
	subscriptionTests := []struct {
		name               string
		subscription       string
		userGroups         string
		expectedModelID    string
		expectedModelCount int
	}{
		{
			name:               "API key - premium subscription",
			subscription:       "premium",
			userGroups:         `["premium-users"]`,
			expectedModelID:    "premium-model",
			expectedModelCount: 1,
		},
		{
			name:               "API key - free subscription",
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
			// Simulate Authorino injecting X-MaaS-Subscription from API key validation
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

	t.Run("user token - single subscription returns all models", func(t *testing.T) {
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

		// User token (no X-MaaS-Subscription header) returns all accessible models
		// User only has access to "free" subscription, so returns that one model
		require.Len(t, response.Data, 1, "Expected one model from accessible subscription")
		assert.Equal(t, "free-model", response.Data[0].ID)
	})

	t.Run("without subscription header - user token returns all models", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err, "Failed to create request")

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["free-users", "premium-users"]`)
		router.ServeHTTP(w, req)

		// User token (no X-MaaS-Subscription header) returns all accessible models
		require.Equal(t, http.StatusOK, w.Code, "Expected status OK")

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err, "Failed to unmarshal response body")

		// User has access to both subscriptions, should return both models
		require.Len(t, response.Data, 2, "Expected both models from both subscriptions")

		modelIDs := make(map[string]bool)
		for _, model := range response.Data {
			modelIDs[model.ID] = true
		}
		assert.True(t, modelIDs["premium-model"], "Should include premium model")
		assert.True(t, modelIDs["free-model"], "Should include free model")
	})

	// Table-driven tests for API key subscription error scenarios
	subscriptionErrorTests := []struct {
		name         string
		subscription string
		userGroups   string
	}{
		{
			name:         "API key - unknown subscription - returns 403",
			subscription: "nonexistent-subscription",
			userGroups:   `["free-users"]`,
		},
		{
			name:         "API key - no access to subscription - returns 403",
			subscription: "premium",
			userGroups:   `["free-users"]`,
		},
	}

	for _, tt := range subscriptionErrorTests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
			require.NoError(t, err, "Failed to create request")

			req.Header.Set("Authorization", "Bearer valid-token")
			req.Header.Set("X-Maas-Subscription", tt.subscription)
			req.Header.Set(constant.HeaderUsername, "test-user@example.com")
			req.Header.Set(constant.HeaderGroup, tt.userGroups)
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusForbidden, w.Code, "Expected 403 Forbidden")

			var errorResponse map[string]any
			err = json.Unmarshal(w.Body.Bytes(), &errorResponse)
			require.NoError(t, err, "Failed to unmarshal error response")

			errorObj, ok := errorResponse["error"].(map[string]any)
			require.True(t, ok, "Expected error object")
			assert.Equal(t, "permission_error", errorObj["type"])
		})
	}
}
func TestListModels_ReturnAllModels(t *testing.T) {
	testLogger := logger.Development()

	// Create mock servers for models
	// Use bare subscription names to match what Authorino injects from API key validation
	model1Server := createMockModelServerWithSubscriptionCheck(t, "model-1", "sub-a")
	model2Server := createMockModelServerWithSubscriptionCheck(t, "model-2", "sub-b")
	model3Server := createMockModelServerWithSubscriptionCheck(t, "model-3", "sub-a")

	// Setup MaaSModelRef lister with three models
	lister := fakeMaaSModelRefLister{
		"test-ns": []*unstructured.Unstructured{
			maasModelRefUnstructured("model-1", "test-ns", model1Server.URL, true, nil),
			maasModelRefUnstructured("model-2", "test-ns", model2Server.URL, true, nil),
			maasModelRefUnstructured("model-3", "test-ns", model3Server.URL, true, nil),
		},
	}

	// Setup subscription lister with display metadata
	createSubscriptionWithMeta := func(name string, groups []string, displayName, description string) *unstructured.Unstructured {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(name)
		sub.SetNamespace(fixtures.TestNamespace)

		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}

		spec := map[string]any{
			"owner": map[string]any{
				"groups": groupSlice,
			},
		}

		_ = unstructured.SetNestedMap(sub.Object, spec, "spec")

		if displayName != "" || description != "" {
			annotations := map[string]string{}
			if displayName != "" {
				annotations[constant.AnnotationDisplayName] = displayName
			}
			if description != "" {
				annotations[constant.AnnotationDescription] = description
			}
			sub.SetAnnotations(annotations)
		}

		return sub
	}

	subscriptionLister := &fakeSubscriptionListerWithMeta{
		subscriptions: []*unstructured.Unstructured{
			createSubscriptionWithMeta("sub-a", []string{"group-a"}, "Subscription A", "Description for A"),
			createSubscriptionWithMeta("sub-b", []string{"group-b"}, "Subscription B", "Description for B"),
		},
	}

	modelMgr, err := models.NewManager(testLogger)
	require.NoError(t, err)

	subscriptionSelector := subscription.NewSelector(testLogger, subscriptionLister)
	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

	config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
	router, _ := fixtures.SetupTestServer(t, config)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)
	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	t.Run("user token - returns all models from all subscriptions", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		// No X-MaaS-Subscription header = user token authentication
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["group-a", "group-b"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, "list", response.Object)
		assert.Len(t, response.Data, 3, "Should return all 3 models from both subscriptions")

		// Verify subscription info is attached
		subscriptionNames := make(map[string]bool)
		for _, model := range response.Data {
			require.NotEmpty(t, model.Subscriptions, "Subscriptions array should not be empty")
			for _, sub := range model.Subscriptions {
				subscriptionNames[sub.Name] = true
			}
		}

		assert.True(t, subscriptionNames["sub-a"], "Should have models from sub-a")
		assert.True(t, subscriptionNames["sub-b"], "Should have models from sub-b")
	})

	t.Run("user token - returns empty list when user has no subscriptions", func(t *testing.T) {
		emptySubscriptionLister := &fakeSubscriptionListerWithMeta{
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithMeta("sub-a", []string{"other-group"}, "", ""),
			},
		}

		subscriptionSelector := subscription.NewSelector(testLogger, emptySubscriptionLister)
		emptyHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

		config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
		router2, _ := fixtures.SetupTestServer(t, config)

		_, cleanup2 := fixtures.StubTokenProviderAPIs(t)
		defer cleanup2()

		tokenHandler2 := token.NewHandler(testLogger, fixtures.TestTenant)
		v1_2 := router2.Group("/v1")
		v1_2.GET("/models", tokenHandler2.ExtractUserInfo(), emptyHandler.ListLLMs)

		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		// No X-MaaS-Subscription header = user token authentication
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["user-group"]`)
		router2.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, "list", response.Object)
		assert.Empty(t, response.Data, "Should return empty list when user has no subscriptions")
	})

	t.Run("user token - attaches subscription metadata to models", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		// No X-MaaS-Subscription header = user token authentication
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["group-a"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Find model-1 which should have sub-a metadata
		var model1 *models.Model
		for i := range response.Data {
			if response.Data[i].ID == "model-1" {
				model1 = &response.Data[i]
				break
			}
		}

		require.NotNil(t, model1, "model-1 should be in response")
		require.NotEmpty(t, model1.Subscriptions, "Subscriptions array should not be empty")
		require.Len(t, model1.Subscriptions, 1, "model-1 should have exactly 1 subscription")
		assert.Equal(t, "sub-a", model1.Subscriptions[0].Name)
		assert.Equal(t, "Subscription A", model1.Subscriptions[0].DisplayName)
		assert.Equal(t, "Description for A", model1.Subscriptions[0].Description)
	})
}

// fakeSubscriptionListerWithMeta implements subscription.Lister with custom subscriptions.
type fakeSubscriptionListerWithMeta struct {
	subscriptions []*unstructured.Unstructured
}

func (f *fakeSubscriptionListerWithMeta) List() ([]*unstructured.Unstructured, error) {
	return f.subscriptions, nil
}

func TestListModels_DeduplicationBySubscription(t *testing.T) {
	testLogger := logger.Development()

	// Create a mock server that responds to both subscriptions
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("shared-model"))
	}))
	t.Cleanup(modelServer.Close)

	// Setup MaaSModelRef lister with one model
	lister := fakeMaaSModelRefLister{
		"test-ns": []*unstructured.Unstructured{
			maasModelRefUnstructured("shared-model", "test-ns", modelServer.URL, true, nil),
		},
	}

	// Setup two subscriptions that both have access to the same model
	createSubscription := func(name string, groups []string) *unstructured.Unstructured {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(name)
		sub.SetNamespace(fixtures.TestNamespace)

		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}

		_ = unstructured.SetNestedMap(sub.Object, map[string]any{
			"owner": map[string]any{
				"groups": groupSlice,
			},
		}, "spec")
		return sub
	}

	subscriptionLister := &fakeSubscriptionListerWithMeta{
		subscriptions: []*unstructured.Unstructured{
			createSubscription("sub-a", []string{"user-group"}),
			createSubscription("sub-b", []string{"user-group"}),
		},
	}

	modelMgr, err := models.NewManager(testLogger)
	require.NoError(t, err)

	subscriptionSelector := subscription.NewSelector(testLogger, subscriptionLister)
	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

	config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
	router, _ := fixtures.SetupTestServer(t, config)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)
	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	t.Run("same model in different subscriptions aggregates into subscriptions array", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("X-Maas-Return-All-Models", "true")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["user-group"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have 1 entry with 2 subscriptions aggregated
		assert.Len(t, response.Data, 1, "Same model instance should aggregate subscriptions into one entry")

		model := response.Data[0]
		assert.Equal(t, "shared-model", model.ID)

		// Should have 2 subscriptions in the array
		require.Len(t, model.Subscriptions, 2, "Model should have 2 subscriptions")

		subscriptionNames := []string{
			model.Subscriptions[0].Name,
			model.Subscriptions[1].Name,
		}

		assert.Contains(t, subscriptionNames, "sub-a")
		assert.Contains(t, subscriptionNames, "sub-b")
	})
}

func TestListModels_DifferentModelRefsWithSameModelID(t *testing.T) {
	testLogger := logger.Development()

	// Create two mock servers that both return the same model ID "gpt-4"
	// but represent different MaaSModelRef instances
	modelServerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("gpt-4"))
	}))
	t.Cleanup(modelServerA.Close)

	modelServerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("gpt-4"))
	}))
	t.Cleanup(modelServerB.Close)

	// Setup MaaSModelRef lister with two different MaaSModelRefs that return same model ID
	lister := fakeMaaSModelRefLister{
		"namespace-a": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-ref", "namespace-a", modelServerA.URL, true, nil),
		},
		"namespace-b": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-ref", "namespace-b", modelServerB.URL, true, nil),
		},
	}

	// Setup single subscription
	createSubscription := func(name string, groups []string) *unstructured.Unstructured {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(name)
		sub.SetNamespace(fixtures.TestNamespace)

		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}

		_ = unstructured.SetNestedMap(sub.Object, map[string]any{
			"owner": map[string]any{
				"groups": groupSlice,
			},
		}, "spec")
		return sub
	}

	subscriptionLister := &fakeSubscriptionListerWithMeta{
		subscriptions: []*unstructured.Unstructured{
			createSubscription("sub-a", []string{"user-group"}),
		},
	}

	modelMgr, err := models.NewManager(testLogger)
	require.NoError(t, err)

	subscriptionSelector := subscription.NewSelector(testLogger, subscriptionLister)
	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

	config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
	router, _ := fixtures.SetupTestServer(t, config)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)
	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	t.Run("different MaaSModelRefs with same model ID but different URLs return separate entries", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("X-Maas-Return-All-Models", "true")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["user-group"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have 2 entries because URLs are different (deduplication by model ID + URL)
		assert.Len(t, response.Data, 2, "Different URLs should create separate entries even with same model ID")

		// Both should have model ID "gpt-4" and subscription "sub-a"
		for _, model := range response.Data {
			assert.Equal(t, "gpt-4", model.ID)
			require.Len(t, model.Subscriptions, 1, "Each model should have 1 subscription")
			assert.Equal(t, "sub-a", model.Subscriptions[0].Name)
		}

		// Verify we have 2 different URLs
		urls := []string{response.Data[0].URL.String(), response.Data[1].URL.String()}
		assert.NotEqual(t, urls[0], urls[1], "Should have different URLs")
	})
}

func TestListModels_DifferentModelRefsWithSameURLAndModelID(t *testing.T) {
	testLogger := logger.Development()

	// Create ONE mock server that both MaaSModelRefs will point to
	sharedModelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("gpt-4"))
	}))
	t.Cleanup(sharedModelServer.Close)

	// Setup MaaSModelRef lister with two different MaaSModelRefs pointing to the SAME URL
	lister := fakeMaaSModelRefLister{
		"namespace-a": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-ref", "namespace-a", sharedModelServer.URL, true, nil),
		},
		"namespace-b": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-another-ref", "namespace-b", sharedModelServer.URL, true, nil),
		},
	}

	// Setup single subscription
	createSubscription := func(name string, groups []string) *unstructured.Unstructured {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(name)
		sub.SetNamespace(fixtures.TestNamespace)

		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}

		_ = unstructured.SetNestedMap(sub.Object, map[string]any{
			"owner": map[string]any{
				"groups": groupSlice,
			},
		}, "spec")
		return sub
	}

	subscriptionLister := &fakeSubscriptionListerWithMeta{
		subscriptions: []*unstructured.Unstructured{
			createSubscription("sub-a", []string{"user-group"}),
		},
	}

	modelMgr, err := models.NewManager(testLogger)
	require.NoError(t, err)

	subscriptionSelector := subscription.NewSelector(testLogger, subscriptionLister)
	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

	config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
	router, _ := fixtures.SetupTestServer(t, config)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)
	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	t.Run("different MaaSModelRefs with same URL and model ID remain separate entries", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("X-Maas-Return-All-Models", "true")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["user-group"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have 2 entries because different MaaSModelRef resources (different ownedBy)
		// even though they have the same URL and model ID
		assert.Len(t, response.Data, 2, "Different MaaSModelRef resources should remain separate entries")

		// Both should have model ID "gpt-4" and same URL but different ownedBy
		for _, model := range response.Data {
			assert.Equal(t, "gpt-4", model.ID)
			assert.Equal(t, sharedModelServer.URL, model.URL.String())
			require.Len(t, model.Subscriptions, 1, "Each model should have 1 subscription")
			assert.Equal(t, "sub-a", model.Subscriptions[0].Name)
			// OwnedBy should be either namespace-a/gpt-4-ref or namespace-b/gpt-4-another-ref
			assert.Contains(t, []string{"namespace-a/gpt-4-ref", "namespace-b/gpt-4-another-ref"}, model.OwnedBy)
		}
	})
}

func TestListModels_DifferentModelRefsWithSameModelIDAndDifferentSubscriptions(t *testing.T) {
	testLogger := logger.Development()

	// Create two mock servers that both return the same model ID "gpt-4"
	// One accessible via sub-a, one via sub-b
	// Use bare subscription names to match what Authorino injects from API key validation
	modelServerA := createMockModelServerWithSubscriptionCheck(t, "gpt-4", "sub-a")
	modelServerB := createMockModelServerWithSubscriptionCheck(t, "gpt-4", "sub-b")

	// Setup MaaSModelRef lister with two different MaaSModelRefs in different namespaces
	lister := fakeMaaSModelRefLister{
		"namespace-a": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-ref", "namespace-a", modelServerA.URL, true, nil),
		},
		"namespace-b": []*unstructured.Unstructured{
			maasModelRefUnstructured("gpt-4-ref", "namespace-b", modelServerB.URL, true, nil),
		},
	}

	// Setup two subscriptions
	createSubscription := func(name string, groups []string) *unstructured.Unstructured {
		sub := &unstructured.Unstructured{}
		sub.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "maas.opendatahub.io",
			Version: "v1alpha1",
			Kind:    "MaaSSubscription",
		})
		sub.SetName(name)
		sub.SetNamespace(fixtures.TestNamespace)

		groupSlice := make([]any, len(groups))
		for i, g := range groups {
			groupSlice[i] = map[string]any{"name": g}
		}

		_ = unstructured.SetNestedMap(sub.Object, map[string]any{
			"owner": map[string]any{
				"groups": groupSlice,
			},
		}, "spec")
		return sub
	}

	subscriptionLister := &fakeSubscriptionListerWithMeta{
		subscriptions: []*unstructured.Unstructured{
			createSubscription("sub-a", []string{"user-group"}),
			createSubscription("sub-b", []string{"user-group"}),
		},
	}

	modelMgr, err := models.NewManager(testLogger)
	require.NoError(t, err)

	subscriptionSelector := subscription.NewSelector(testLogger, subscriptionLister)
	modelsHandler := handlers.NewModelsHandler(testLogger, modelMgr, subscriptionSelector, lister)

	config := fixtures.TestServerConfig{Objects: []runtime.Object{}}
	router, _ := fixtures.SetupTestServer(t, config)

	_, cleanup := fixtures.StubTokenProviderAPIs(t)
	defer cleanup()

	tokenHandler := token.NewHandler(testLogger, fixtures.TestTenant)
	v1 := router.Group("/v1")
	v1.GET("/models", tokenHandler.ExtractUserInfo(), modelsHandler.ListLLMs)

	t.Run("different MaaSModelRefs with same model ID but different URLs return separate entries", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		require.NoError(t, err)

		req.Header.Set("Authorization", "Bearer valid-token")
		req.Header.Set("X-Maas-Return-All-Models", "true")
		req.Header.Set(constant.HeaderUsername, "test-user@example.com")
		req.Header.Set(constant.HeaderGroup, `["user-group"]`)
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)

		var response pagination.Page[models.Model]
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should have 2 entries because URLs are different (even though model ID is same)
		assert.Len(t, response.Data, 2, "Different URLs should create separate entries even with same model ID")

		// Both should have model ID "gpt-4" but different URLs and subscriptions
		modelsByURL := make(map[string]models.Model)
		for _, model := range response.Data {
			assert.Equal(t, "gpt-4", model.ID, "Both entries should have model ID gpt-4")
			require.Len(t, model.Subscriptions, 1, "Each model should have exactly 1 subscription")
			modelsByURL[model.URL.String()] = model
		}

		// Verify we have 2 different URLs
		assert.Len(t, modelsByURL, 2, "Should have 2 different URLs")

		// Verify each has the correct subscription
		subscriptionNames := make(map[string]bool)
		for _, model := range response.Data {
			subscriptionNames[model.Subscriptions[0].Name] = true
		}
		assert.True(t, subscriptionNames["sub-a"], "Should have model with sub-a")
		assert.True(t, subscriptionNames["sub-b"], "Should have model with sub-b")
	})
}
