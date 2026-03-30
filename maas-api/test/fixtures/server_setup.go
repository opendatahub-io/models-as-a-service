package fixtures

import (
	"fmt"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kservelistersv1alpha1 "github.com/kserve/kserve/pkg/client/listers/serving/v1alpha1"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func ToRuntimeObjects[T runtime.Object](items []T) []runtime.Object {
	result := make([]runtime.Object, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}

// TokenReviewScenario defines how TokenReview should respond for a given token.
type TokenReviewScenario struct {
	Authenticated bool
	UserInfo      authv1.UserInfo
	ShouldError   bool
	ErrorMessage  string
}

// TestServerConfig holds configuration for test server setup.
type TestServerConfig struct {
	Objects       []runtime.Object
	TestNamespace string
	TestTenant    string
}

type TestClients struct {
	K8sClient                 kubernetes.Interface
	LLMInferenceServiceLister kservelistersv1alpha1.LLMInferenceServiceLister
	HTTPRouteLister           gatewaylisters.HTTPRouteLister
}

// TestComponents holds common test components.
type TestComponents struct {
	Clientset *k8sfake.Clientset
}

// SetupTestServer creates a test server with base configuration.
func SetupTestServer(_ *testing.T, config TestServerConfig) (*gin.Engine, *TestClients) {
	gin.SetMode(gin.TestMode)

	if config.TestNamespace == "" {
		config.TestNamespace = TestNamespace
	}
	if config.TestTenant == "" {
		config.TestTenant = TestTenant
	}

	var k8sObjects []runtime.Object
	var llmIsvcs []*kservev1alpha1.LLMInferenceService

	for _, obj := range config.Objects {
		gvk := obj.GetObjectKind().GroupVersionKind()
		switch {
		case gvk.Group == "serving.kserve.io" && gvk.Kind == "LLMInferenceService":
			if llm, ok := obj.(*kservev1alpha1.LLMInferenceService); ok {
				llmIsvcs = append(llmIsvcs, llm)
			}
		default:
			k8sObjects = append(k8sObjects, obj)
		}
	}

	k8sClient := k8sfake.NewClientset(k8sObjects...)
	clients := &TestClients{
		K8sClient:                 k8sClient,
		LLMInferenceServiceLister: NewLLMInferenceServiceLister(ToRuntimeObjects(llmIsvcs)...),
		HTTPRouteLister:           NewHTTPRouteLister(),
	}

	return gin.New(), clients
}

// StubTokenProviderAPIs creates common test components for token tests.
func StubTokenProviderAPIs(_ *testing.T) (*k8sfake.Clientset, func()) {
	fakeClient := k8sfake.NewClientset()

	// Stub ServiceAccount token creation for tests
	StubServiceAccountTokenCreation(fakeClient)

	cleanup := func() {}

	return fakeClient, cleanup
}

// SetupTestRouter creates a test router with token endpoints.
// Returns the router and a cleanup function that must be called to close the store and remove the temp DB file.
func SetupTestRouter() (*gin.Engine, func() error) {
	testLogger := logger.Development()

	gin.SetMode(gin.TestMode)
	router := gin.New()

	tokenHandler := token.NewHandler(testLogger, "test")

	protected := router.Group("/v1")
	protected.Use(tokenHandler.ExtractUserInfo())

	cleanup := func() error {
		return nil
	}

	return router, cleanup
}

// StubServiceAccountTokenCreation sets up ServiceAccount token creation mocking for tests.
func StubServiceAccountTokenCreation(clientset kubernetes.Interface) {
	fakeClient, ok := clientset.(*k8sfake.Clientset)
	if !ok {
		panic("StubServiceAccountTokenCreation: clientset is not a *k8sfake.Clientset")
	}

	fakeClient.PrependReactor("create", "serviceaccounts/token", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateAction)
		if !ok {
			return true, nil, fmt.Errorf("expected CreateAction, got %T", action)
		}
		tokenRequest, ok := createAction.GetObject().(*authv1.TokenRequest)
		if !ok {
			return true, nil, fmt.Errorf("expected TokenRequest, got %T", createAction.GetObject())
		}

		// Generate valid JWT
		claims := jwt.MapClaims{
			"jti": fmt.Sprintf("mock-jti-%d", time.Now().UnixNano()),
			"iat": time.Now().Unix(),
			"exp": time.Now().Add(time.Hour).Unix(),
			"sub": "system:serviceaccount:test-namespace:test-sa",
		}

		signedToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("secret"))
		if err != nil {
			panic(fmt.Sprintf("failed to sign JWT token in test fixture: %v", err))
		}

		tokenRequest.Status = authv1.TokenRequestStatus{
			Token:               signedToken,
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
		}

		return true, tokenRequest, nil
	})
}
