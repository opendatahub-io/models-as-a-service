package models_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

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

// TestListAvailableLLMs_AlwaysAllowed tests gateway/route matching logic
// by using a mock server that always returns 200 (authorized).
func TestListAvailableLLMs_AlwaysAllowed(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	// Mock server that always allows access
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer authServer.Close()

	tests := []struct {
		name        string
		llmServices []*kservev1alpha1.LLMInferenceService
		httpRoutes  []*gwapiv1.HTTPRoute
		expectMatch []string
	}{
		{
			name: "direct gateway reference with addresses",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						AddressStatus: duckv1.AddressStatus{
							Addresses: makeAddresses("gateway-internal", authServer.URL),
						},
					},
				},
			},
			expectMatch: []string{"llm-direct"},
		},
		{
			name: "inline HTTPRoute spec",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						AddressStatus: duckv1.AddressStatus{
							Addresses: makeAddresses("gateway-external", authServer.URL),
						},
					},
				},
			},
			expectMatch: []string{"llm-inline"},
		},
		{
			name: "referenced HTTPRoute",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						URL: mustParseURL(authServer.URL),
					},
				},
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
			name: "managed HTTPRoute",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						URL: mustParseURL(authServer.URL),
					},
				},
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
			name: "multiple gateway references with maas-gateway",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						URL: mustParseURL(authServer.URL),
					},
				},
			},
			expectMatch: []string{"llm-multi-gw"},
		},
		{
			name: "no match different gateway",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						URL: mustParseURL(authServer.URL),
					},
				},
			},
			expectMatch: []string{},
		},
		{
			name: "no match referenced HTTPRoute with different gateway",
			llmServices: []*kservev1alpha1.LLMInferenceService{
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
						URL: mustParseURL(authServer.URL),
					},
				},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, errMgr := models.NewManager(
				testLogger,
				fixtures.NewInferenceServiceLister(),
				fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects(tt.llmServices)...),
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

// TestListAvailableLLMs_Authorization tests that authorization is enforced correctly.
func TestListAvailableLLMs_Authorization(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	// Create mock HTTP server to simulate gateway authorization responses.
	// The auth check uses GET /v1/models to verify access.
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "Bearer valid-token" {
			w.WriteHeader(http.StatusOK)
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
			fixtures.NewInferenceServiceLister(),
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
			fixtures.NewInferenceServiceLister(),
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

func TestListAvailableLLMs_ExternalAddressTriedFirst(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	var callOrder []string
	var callCount atomic.Int32

	internalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "internal")
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internalServer.Close()

	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, "external")
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
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
		fixtures.NewInferenceServiceLister(),
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
		w.WriteHeader(http.StatusOK)
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
		fixtures.NewInferenceServiceLister(),
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Len(t, authorizedModels, 1)
	assert.Equal(t, []string{"external", "internal"}, callOrder, "Should try external first, fallback to internal on 503")
}

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
		w.WriteHeader(http.StatusOK)
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
		fixtures.NewInferenceServiceLister(),
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
		fixtures.NewInferenceServiceLister(),
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

func TestListAvailableLLMs_MultiTenantSharedModel(t *testing.T) {
	testLogger := logger.Development()
	gateway := models.GatewayRef{Name: "maas-gateway", Namespace: "gateway-ns"}

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer authServer.Close()

	llmService := &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "llama3-8b-shared", Namespace: "shared-models"},
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
					"gateway-internal", authServer.URL,
					"gateway-external", authServer.URL,
					"gateway-internal", authServer.URL,
					"gateway-external", authServer.URL,
				),
			},
		},
	}

	manager, errMgr := models.NewManager(
		testLogger,
		fixtures.NewInferenceServiceLister(),
		fixtures.NewLLMInferenceServiceLister(fixtures.ToRuntimeObjects([]*kservev1alpha1.LLMInferenceService{llmService})...),
		fixtures.NewHTTPRouteLister(),
		gateway,
	)
	require.NoError(t, errMgr)

	authorizedModels, err := manager.ListAvailableLLMs(t.Context(), "any-token")
	require.NoError(t, err)

	assert.Len(t, authorizedModels, 1)
	assert.Equal(t, "llama3-8b-shared", authorizedModels[0].ID)
	assert.Contains(t, authorizedModels[0].URL.String(), authServer.URL)
}

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
		w.WriteHeader(http.StatusOK)
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
		fixtures.NewInferenceServiceLister(),
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
