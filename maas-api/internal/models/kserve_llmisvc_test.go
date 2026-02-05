package models_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

// TestListAvailableLLMs_AlwaysAllowed tests gateway/route matching logic
// by using a mock server that always returns 200 (authorized) with a valid /v1/models response.
func TestListAvailableLLMs_AlwaysAllowed(t *testing.T) { //nolint:maintidx // table-driven test
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	tests := []struct {
		name           string
		serverModelIDs []string // model IDs the mock server will return
		llmServices    func(serverURL string) []*kservev1alpha1.LLMInferenceService
		httpRoutes     []*gwapiv1.HTTPRoute
		expectMatch    []string
	}{
		{
			name:           "direct gateway reference",
			serverModelIDs: []string{"llm-direct"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-direct", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Gateway: &kservev1alpha1.GatewaySpec{
									Refs: []kservev1alpha1.UntypedObjectReference{
										{Name: "maas-gateway", Namespace: "gateway-ns"},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			expectMatch: []string{"llm-direct"},
		},
		{
			name:           "inline HTTPRoute spec",
			serverModelIDs: []string{"llm-inline"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-inline", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Route: &kservev1alpha1.GatewayRoutesSpec{
									HTTP: &kservev1alpha1.HTTPRouteSpec{
										Spec: &gwapiv1.HTTPRouteSpec{
											CommonRouteSpec: gwapiv1.CommonRouteSpec{
												ParentRefs: []gwapiv1.ParentReference{
													{Name: "maas-gateway", Namespace: ptr.To(gwapiv1.Namespace("gateway-ns"))},
												},
											},
										},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			expectMatch: []string{"llm-inline"},
		},
		{
			name:           "referenced HTTPRoute",
			serverModelIDs: []string{"llm-ref"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-ref", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Route: &kservev1alpha1.GatewayRoutesSpec{
									HTTP: &kservev1alpha1.HTTPRouteSpec{
										Refs: []corev1.LocalObjectReference{{Name: "my-route"}},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			httpRoutes: []*gwapiv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "test-ns"},
					Spec: gwapiv1.HTTPRouteSpec{
						CommonRouteSpec: gwapiv1.CommonRouteSpec{
							ParentRefs: []gwapiv1.ParentReference{
								{Name: "maas-gateway", Namespace: ptr.To(gwapiv1.Namespace("gateway-ns"))},
							},
						},
					},
				},
			},
			expectMatch: []string{"llm-ref"},
		},
		{
			name:           "managed HTTPRoute",
			serverModelIDs: []string{"llm-managed"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-managed", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Route: &kservev1alpha1.GatewayRoutesSpec{
									HTTP: &kservev1alpha1.HTTPRouteSpec{},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			httpRoutes: []*gwapiv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "managed-route",
						Namespace: "test-ns",
						Labels: map[string]string{
							"app.kubernetes.io/component": "llminferenceservice-router",
							"app.kubernetes.io/name":      "llm-managed",
							"app.kubernetes.io/part-of":   "llminferenceservice",
						},
					},
					Spec: gwapiv1.HTTPRouteSpec{
						CommonRouteSpec: gwapiv1.CommonRouteSpec{
							ParentRefs: []gwapiv1.ParentReference{
								{Name: "maas-gateway", Namespace: ptr.To(gwapiv1.Namespace("gateway-ns"))},
							},
						},
					},
				},
			},
			expectMatch: []string{"llm-managed"},
		},
		{
			name:           "multiple gateway references with maas-gateway",
			serverModelIDs: []string{"llm-multi-gw"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-multi-gw", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Gateway: &kservev1alpha1.GatewaySpec{
									Refs: []kservev1alpha1.UntypedObjectReference{
										{Name: "first-gateway", Namespace: "gateway-ns"},
										{Name: "second-gateway", Namespace: "gateway-ns"},
										{Name: "maas-gateway", Namespace: "gateway-ns"},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			expectMatch: []string{"llm-multi-gw"},
		},
		{
			name:           "no match different gateway",
			serverModelIDs: []string{"llm-different"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-different", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Gateway: &kservev1alpha1.GatewaySpec{
									Refs: []kservev1alpha1.UntypedObjectReference{
										{Name: "other-gateway", Namespace: "gateway-ns"},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			expectMatch: []string{},
		},
		{
			name:           "no match referenced HTTPRoute with different gateway",
			serverModelIDs: []string{"llm-ref-different"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-ref-different", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Route: &kservev1alpha1.GatewayRoutesSpec{
									HTTP: &kservev1alpha1.HTTPRouteSpec{
										Refs: []corev1.LocalObjectReference{{Name: "different-route"}},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			httpRoutes: []*gwapiv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "different-route", Namespace: "test-ns"},
					Spec: gwapiv1.HTTPRouteSpec{
						CommonRouteSpec: gwapiv1.CommonRouteSpec{
							ParentRefs: []gwapiv1.ParentReference{
								{Name: "other-gateway", Namespace: ptr.To(gwapiv1.Namespace("gateway-ns"))},
							},
						},
					},
				},
			},
			expectMatch: []string{},
		},
		{
			name:           "discovers model with aliases from single service",
			serverModelIDs: []string{"base-model", "model-alias-1", "model-alias-2"},
			llmServices: func(serverURL string) []*kservev1alpha1.LLMInferenceService {
				return []*kservev1alpha1.LLMInferenceService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "llm-multi-model", Namespace: "test-ns"},
						Spec: kservev1alpha1.LLMInferenceServiceSpec{
							Router: &kservev1alpha1.RouterSpec{
								Gateway: &kservev1alpha1.GatewaySpec{
									Refs: []kservev1alpha1.UntypedObjectReference{
										{Name: "maas-gateway", Namespace: "gateway-ns"},
									},
								},
							},
						},
						Status: kservev1alpha1.LLMInferenceServiceStatus{
							URL: mustParseURL(serverURL),
						},
					},
				}
			},
			// First discovered model becomes canonical, rest become aliases
			expectMatch: []string{"base-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock server that returns the specified model IDs
			mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(makeModelsResponse(tt.serverModelIDs...))
			}))
			defer mockServer.Close()

			llmServices := tt.llmServices(mockServer.URL)
			manager, errMgr := models.NewManager(
				testLogger,
				fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects(llmServices)...),
				fixtures.NewHTTPRouteLister(fixtures.ToRuntimeObjects(tt.httpRoutes)...),
				gateway,
			)
			require.NoError(t, errMgr)

			availableModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
			require.NoError(t, err)

			var actualNames []string
			for _, model := range availableModels {
				actualNames = append(actualNames, model.ID)
			}

			assert.ElementsMatch(t, tt.expectMatch, actualNames)
		})
	}
}

// TestListAvailableLLMs_MultiModelDiscovery tests scenarios where a single
// inference service serves multiple models (e.g., via served model names/aliases).
func TestListAvailableLLMs_MultiModelDiscovery(t *testing.T) { //nolint:maintidx // comprehensive test suite
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	// Helper to create a standard LLMInferenceService with Ready status
	makeLLMService := func(name, namespace, serverURL string, annotations map[string]string) *kservev1alpha1.LLMInferenceService {
		now := metav1.NewTime(time.Now())
		svc := &kservev1alpha1.LLMInferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         namespace,
				Annotations:       annotations,
				CreationTimestamp: now,
			},
			Spec: kservev1alpha1.LLMInferenceServiceSpec{
				Router: &kservev1alpha1.RouterSpec{
					Gateway: &kservev1alpha1.GatewaySpec{
						Refs: []kservev1alpha1.UntypedObjectReference{
							{Name: "maas-gateway", Namespace: "gateway-ns"},
						},
					},
				},
			},
			Status: kservev1alpha1.LLMInferenceServiceStatus{
				URL: mustParseURL(serverURL),
				Status: duckv1.Status{
					ObservedGeneration: 1,
					Conditions: duckv1.Conditions{
						{Type: apis.ConditionReady, Status: corev1.ConditionTrue, LastTransitionTime: apis.VolatileTime{Inner: now}},
					},
				},
			},
		}
		svc.Generation = 1
		return svc
	}

	t.Run("vLLM with multiple served model names", func(t *testing.T) {
		// Simulates a vLLM deployment with multiple served model names (aliases)
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// vLLM /v1/models response with multiple served model names
			response := `{
				"object": "list",
				"data": [
					{"id": "meta-llama/Llama-2-7b-hf", "object": "model", "created": 1700000000, "owned_by": "vllm"},
					{"id": "llama-2-sql", "object": "model", "created": 1700000001, "owned_by": "vllm"},
					{"id": "llama-2-chat", "object": "model", "created": 1700000002, "owned_by": "vllm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("llama-2-7b-service", "model-serving", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1, "Expected 1 canonical model with aliases")

		m := discoveredModels[0]
		// First discovered model becomes canonical
		assert.Equal(t, "meta-llama/Llama-2-7b-hf", m.ID, "First model should be canonical")
		assert.ElementsMatch(t, []string{"llama-2-sql", "llama-2-chat"}, m.Aliases, "Other models should be aliases")

		// Model should have service context
		assert.Equal(t, mockServer.URL, m.URL.String(), "Model should have the service URL")
		assert.True(t, m.Ready, "Model should inherit Ready state from service")
		// Server-provided owned_by is preserved
		assert.Equal(t, "vllm", m.OwnedBy, "OwnedBy from server response should be preserved")
	})

	t.Run("vLLM with many model aliases", func(t *testing.T) {
		// Simulates a vLLM deployment with many served model names for different use cases
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "mistralai/Mistral-7B-v0.1", "object": "model", "created": 1700000000, "owned_by": "vllm"},
					{"id": "mistral-code-review", "object": "model", "created": 1700000001, "owned_by": "vllm"},
					{"id": "mistral-summarization", "object": "model", "created": 1700000002, "owned_by": "vllm"},
					{"id": "mistral-translation-en-es", "object": "model", "created": 1700000003, "owned_by": "vllm"},
					{"id": "mistral-translation-en-de", "object": "model", "created": 1700000004, "owned_by": "vllm"},
					{"id": "mistral-customer-support", "object": "model", "created": 1700000005, "owned_by": "vllm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("mistral-7b-service", "ml-team", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1, "Expected 1 canonical model")

		m := discoveredModels[0]
		assert.Equal(t, "mistralai/Mistral-7B-v0.1", m.ID, "First model should be canonical")
		assert.Len(t, m.Aliases, 5, "Expected 5 aliases")
		assert.ElementsMatch(t, []string{
			"mistral-code-review", "mistral-summarization", "mistral-translation-en-es",
			"mistral-translation-en-de", "mistral-customer-support",
		}, m.Aliases)
	})

	t.Run("multiple services each with multiple models", func(t *testing.T) {
		// Simulates multiple LLMInferenceServices, each serving multiple models
		mockServer1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "llama-2-7b", "object": "model", "created": 1700000000, "owned_by": "vllm"},
					{"id": "llama-2-sql", "object": "model", "created": 1700000001, "owned_by": "vllm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer1.Close()

		mockServer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "mistral-7b", "object": "model", "created": 1700000000, "owned_by": "vllm"},
					{"id": "mistral-code", "object": "model", "created": 1700000001, "owned_by": "vllm"},
					{"id": "mistral-chat", "object": "model", "created": 1700000002, "owned_by": "vllm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer2.Close()

		llmService1 := makeLLMService("llama-service", "ns-1", mockServer1.URL, nil)
		llmService2 := makeLLMService("mistral-service", "ns-2", mockServer2.URL, nil)

		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService1, llmService2),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 2, "Expected 2 canonical models: 1 from each service")

		// Find models by canonical ID
		var llamaModel, mistralModel *models.Model
		for i := range discoveredModels {
			switch discoveredModels[i].ID {
			case "llama-2-7b":
				llamaModel = &discoveredModels[i]
			case "mistral-7b":
				mistralModel = &discoveredModels[i]
			}
		}
		require.NotNil(t, llamaModel, "Expected llama canonical model")
		require.NotNil(t, mistralModel, "Expected mistral canonical model")

		// Verify aliases
		assert.ElementsMatch(t, []string{"llama-2-sql"}, llamaModel.Aliases)
		assert.ElementsMatch(t, []string{"mistral-code", "mistral-chat"}, mistralModel.Aliases)

		// Verify models are associated with correct service URLs
		assert.Equal(t, mockServer1.URL, llamaModel.URL.String())
		assert.Equal(t, mockServer2.URL, mistralModel.URL.String())

		// Server-provided owned_by is preserved
		assert.Equal(t, "vllm", llamaModel.OwnedBy, "Server-provided OwnedBy should be preserved")
		assert.Equal(t, "vllm", mistralModel.OwnedBy, "Server-provided OwnedBy should be preserved")
	})

	t.Run("service metadata is inherited by discovered model", func(t *testing.T) {
		// Test that Details from LLMInferenceService annotations are applied to the canonical model
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "granite-7b-base", "object": "model", "created": 1700000000, "owned_by": "ibm"},
					{"id": "granite-7b-code", "object": "model", "created": 1700000001, "owned_by": "ibm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		annotations := map[string]string{
			constant.AnnotationGenAIUseCase: "Code Generation",
			constant.AnnotationDescription:  "IBM Granite model family for code",
			constant.AnnotationDisplayName:  "Granite Code Models",
		}
		llmService := makeLLMService("granite-service", "ibm-models", mockServer.URL, annotations)

		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1)

		m := discoveredModels[0]
		assert.Equal(t, "granite-7b-base", m.ID, "First model should be canonical")
		assert.ElementsMatch(t, []string{"granite-7b-code"}, m.Aliases)

		// Model should inherit Details from the service
		require.NotNil(t, m.Details, "Expected Details to be set")
		assert.Equal(t, "Code Generation", m.Details.GenAIUseCase)
		assert.Equal(t, "IBM Granite model family for code", m.Details.Description)
		assert.Equal(t, "Granite Code Models", m.Details.DisplayName)
	})

	t.Run("empty model list from server", func(t *testing.T) {
		// Edge case: server returns empty model list
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{"object": "list", "data": []}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("empty-service", "test-ns", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		assert.Empty(t, discoveredModels, "Expected no models when server returns empty list")
	})

	t.Run("405 Method Not Allowed uses spec.model.name as fallback model ID", func(t *testing.T) {
		// When server returns 405, auth succeeded but /v1/models endpoint is not supported.
		// Should return a model using spec.model.name as best-effort fallback.
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}))
		defer mockServer.Close()

		annotations := map[string]string{
			constant.AnnotationDescription: "A model without /v1/models support",
			constant.AnnotationDisplayName: "Fallback Model",
		}
		llmService := makeLLMService("my-llm-service", "test-ns", mockServer.URL, annotations)
		specModelName := "granite-3b-instruct"
		llmService.Spec.Model.Name = &specModelName

		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1, "Should return one model using spec.model.name as fallback")
		assert.Equal(t, "granite-3b-instruct", discoveredModels[0].ID, "Model ID should be from spec.model.name")
		assert.Equal(t, "test-ns", discoveredModels[0].OwnedBy, "OwnedBy should be set from namespace")
		assert.NotZero(t, discoveredModels[0].Created, "Created should be set from service creation timestamp")
		// Verify metadata is still enriched from LLMInferenceService annotations
		require.NotNil(t, discoveredModels[0].Details)
		assert.Equal(t, "A model without /v1/models support", discoveredModels[0].Details.Description)
		assert.Equal(t, "Fallback Model", discoveredModels[0].Details.DisplayName)
	})

	t.Run("405 Method Not Allowed falls back to service name when spec.model.name is empty", func(t *testing.T) {
		// When spec.model.name is not set, fall back to LLMInferenceService name
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}))
		defer mockServer.Close()

		llmService := makeLLMService("my-fallback-service", "test-ns", mockServer.URL, nil)
		// spec.model.name is not set

		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1)
		assert.Equal(t, "my-fallback-service", discoveredModels[0].ID, "Model ID should fall back to service name")
	})

	t.Run("model timestamps from server response are preserved", func(t *testing.T) {
		// Verify that Created timestamps from the server response are preserved
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "model-with-timestamp", "object": "model", "created": 1609459200, "owned_by": "test"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("timestamp-service", "test-ns", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1)
		// 1609459200 = 2021-01-01 00:00:00 UTC
		assert.Equal(t, int64(1609459200), discoveredModels[0].Created, "Server-provided timestamp should be preserved")
	})

	t.Run("model without timestamp uses service creation time", func(t *testing.T) {
		// When server doesn't provide timestamp, use service creation time as fallback
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "model-no-timestamp", "object": "model", "owned_by": "test"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("no-timestamp-service", "test-ns", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1)
		// Should use service creation timestamp as fallback
		// The important thing is the discovery completes without error
		assert.NotZero(t, discoveredModels[0].Created, "Created should be set from service creation timestamp")
	})

	t.Run("namespace is used as OwnedBy fallback when server returns empty", func(t *testing.T) {
		// When server returns empty owned_by, the namespace should be used as fallback
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "model-no-owner", "object": "model", "created": 1700000000, "owned_by": ""},
					{"id": "model-null-owner", "object": "model", "created": 1700000001}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer mockServer.Close()

		llmService := makeLLMService("fallback-test-service", "my-namespace", mockServer.URL, nil)
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(llmService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1)

		m := discoveredModels[0]
		assert.Equal(t, "model-no-owner", m.ID)
		assert.ElementsMatch(t, []string{"model-null-owner"}, m.Aliases)
		// Model should use namespace as OwnedBy since server returned empty
		assert.Equal(t, "my-namespace", m.OwnedBy, "Namespace should be used as OwnedBy fallback")
	})

	t.Run("partial authorization - one service authorized, one not", func(t *testing.T) {
		// User has access to one service but not another
		authorizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			response := `{
				"object": "list",
				"data": [
					{"id": "authorized-model", "object": "model", "created": 1700000000, "owned_by": "vllm"}
				]
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
		}))
		defer authorizedServer.Close()

		unauthorizedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer unauthorizedServer.Close()

		authService := makeLLMService("auth-service", "ns-1", authorizedServer.URL, nil)
		unauthService := makeLLMService("unauth-service", "ns-2", unauthorizedServer.URL, nil)

		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(authService, unauthService),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		discoveredModels, err := manager.ListAvailableLLMs(t.Context(), "partial-access-token")
		require.NoError(t, err)

		require.Len(t, discoveredModels, 1, "Should only return models from authorized service")
		assert.Equal(t, "authorized-model", discoveredModels[0].ID)
	})
}

// TestListAvailableLLMs_Authorization tests that authorization is enforced correctly.
func TestListAvailableLLMs_Authorization(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	// Create mock HTTP server to simulate gateway authorization responses.
	// The auth check uses GET /v1/models to verify access and discover models.
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if authHeader == "Bearer valid-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(makeModelsResponse("test-llm"))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer authServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-llm", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: makeAddresses("gateway-internal", authServer.URL),
			},
		},
	}

	t.Run("user with valid token has access", func(t *testing.T) {
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "valid-token")
		require.NoError(t, err)

		assert.Len(t, authorizedModels, 1, "Expected 1 authorized model")
		assert.Equal(t, "test-llm", authorizedModels[0].ID)
	})

	t.Run("user with invalid token has no access", func(t *testing.T) {
		manager, errMgr := models.NewManager(
			testLogger,
			fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
			fixtures.NewHTTPRouteLister(),
			gateway,
		)
		require.NoError(t, errMgr)

		authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "invalid-token")
		require.NoError(t, err)

		assert.Empty(t, authorizedModels, "Expected 0 authorized models for invalid token")
	})
}

// TestListAvailableLLMs_ExternalAddressTriedFirst tests that external addresses
// are tried before internal addresses when discovering models.
func TestListAvailableLLMs_ExternalAddressTriedFirst(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	var callOrder []string
	var callCount atomic.Int32

	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "internal")
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("shared-model"))
	}))
	defer internalServer.Close()

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "external")
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("shared-model"))
	}))
	defer externalServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-model", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: makeAddresses(
					"gateway-internal", internalServer.URL,
					"gateway-external", externalServer.URL,
				),
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Len(t, authorizedModels, 1)
	assert.Equal(t, "shared-model", authorizedModels[0].ID)
	assert.Equal(t, int32(1), callCount.Load(), "Should only call external (first in priority)")
	assert.Equal(t, []string{"external"}, callOrder)
}

// TestListAvailableLLMs_FallbackToInternal tests that when external address fails,
// the internal address is tried as fallback.
func TestListAvailableLLMs_FallbackToInternal(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	var callOrder []string

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "external")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer externalServer.Close()

	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "internal")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("shared-model"))
	}))
	defer internalServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-model", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: makeAddresses(
					"gateway-internal", internalServer.URL,
					"gateway-external", externalServer.URL,
				),
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Len(t, authorizedModels, 1)
	// Note: Due to retry backoff on 503, external may be called multiple times before falling back
	assert.Contains(t, callOrder, "external", "Should try external first")
	assert.Contains(t, callOrder, "internal", "Should fallback to internal on 503")
}

// TestListAvailableLLMs_DenialFromOneGatewaySuccessFromAnother tests multi-tenant
// scenarios where different gateways have different AuthPolicies.
func TestListAvailableLLMs_DenialFromOneGatewaySuccessFromAnother(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	var callOrder []string

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "external")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer externalServer.Close()

	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "internal")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("shared-model"))
	}))
	defer internalServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-model", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: makeAddresses(
					"gateway-internal", internalServer.URL,
					"gateway-external", externalServer.URL,
				),
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Len(t, authorizedModels, 1, "Should succeed via internal after external denied")
	assert.Equal(t, []string{"external", "internal"}, callOrder, "Should try all addresses")
}

// TestListAvailableLLMs_AllAddressesDenied tests that when all addresses deny access,
// the model is not returned.
func TestListAvailableLLMs_AllAddressesDenied(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	var callCount atomic.Int32

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer externalServer.Close()

	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer internalServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "denied-model", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: makeAddresses(
					"gateway-internal", internalServer.URL,
					"gateway-external", externalServer.URL,
				),
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Empty(t, authorizedModels, "Should be denied when all addresses deny")
	assert.Equal(t, int32(2), callCount.Load(), "Should try all addresses before denying")
}

// TestListAvailableLLMs_ModelURLUsesAccessibleAddress tests that the returned model
// URL is the address that was actually accessible.
func TestListAvailableLLMs_ModelURLUsesAccessibleAddress(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	// External server that denies access - simulates inaccessible external endpoint
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer externalServer.Close()

	// Internal server that allows access
	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeModelsResponse("test-model"))
	}))
	defer internalServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "test-ns"},
		Spec: kservev1alpha1.LLMInferenceServiceSpec{
			Router: &kservev1alpha1.RouterSpec{
				Gateway: &kservev1alpha1.GatewaySpec{
					Refs: []kservev1alpha1.UntypedObjectReference{
						{Name: "maas-gateway", Namespace: "gateway-ns"},
					},
				},
			},
		},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: []duckv1.Addressable{
					{Name: ptr.To("gateway-external"), URL: mustParseURL(externalServer.URL)},
					{Name: ptr.To("gateway-internal"), URL: mustParseURL(internalServer.URL)},
				},
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	require.Len(t, authorizedModels, 1)
	assert.Equal(t, internalServer.URL, authorizedModels[0].URL.String(),
		"Model URL should use the accessible address (internal, since external denied)")
}

// --- Test utilities ---

// modelsListResponse mimics the OpenAI /v1/models response structure for tests.
type modelsListResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// makeModelsResponse creates a JSON response for /v1/models endpoint.
func makeModelsResponse(modelIDs ...string) []byte {
	resp := modelsListResponse{
		Object: "list",
		Data:   make([]modelEntry, 0, len(modelIDs)),
	}
	for _, id := range modelIDs {
		resp.Data = append(resp.Data, modelEntry{
			ID:      id,
			Object:  "model",
			Created: 1700000000,
			OwnedBy: "test",
		})
	}
	data, err := json.Marshal(resp)
	if err != nil {
		panic("test setup failed: " + err.Error())
	}
	return data
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

// makeAddresses creates a slice of duckv1.Addressable from name/URL pairs.
func makeAddresses(pairs ...string) []duckv1.Addressable {
	if len(pairs)%2 != 0 {
		panic("makeAddresses requires name/URL pairs")
	}
	var addresses []duckv1.Addressable
	for i := 0; i < len(pairs); i += 2 {
		name := pairs[i]
		urlStr := pairs[i+1]
		addr := duckv1.Addressable{URL: mustParseURL(urlStr)}
		if name != "" {
			addr.Name = ptr.To(name)
		}
		addresses = append(addresses, addr)
	}
	return addresses
}
