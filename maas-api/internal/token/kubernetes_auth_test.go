package token_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// newK8sRouter builds a router with the Kubernetes-auth-enabled strict
// middleware and a fake clientset whose TokenReviews are answered by the
// given reactor result.
func newK8sRouter(t *testing.T, reviewErr error, review *authv1.TokenReviewStatus) (*gin.Engine, *fake.Clientset) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if reviewErr != nil {
			return true, nil, reviewErr
		}
		return true, &authv1.TokenReview{Status: *review}, nil
	})

	h := token.NewHandler(logger.Development(), "test-tenant").WithKubeClient(client)
	router := gin.New()
	router.Use(h.ExtractUserInfo(token.AuthModeGatewayOrKubernetes))
	router.GET("/test", func(c *gin.Context) {
		user, exists := c.Get("user")
		if !exists {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "user not found in context"})
			return
		}
		c.JSON(http.StatusOK, user)
	})
	return router, client
}

func authenticatedReview() *authv1.TokenReviewStatus {
	return &authv1.TokenReviewStatus{
		Authenticated: true,
		User: authv1.UserInfo{
			Username: "kube-user",
			Groups:   []string{"system:authenticated", "cluster-admins"},
		},
	}
}

func TestExtractUserInfo_KubernetesFallback(t *testing.T) {
	t.Run("valid bearer token authenticates via TokenReview", func(t *testing.T) {
		router, _ := newK8sRouter(t, nil, authenticatedReview())

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer sha256~oc-token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		var body token.UserContext
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "kube-user", body.Username)
		assert.Equal(t, "test-tenant", body.Tenant, "tenant should come from handler config")
		assert.Equal(t, token.AuthSourceK8s, body.AuthSource)
		assert.Contains(t, body.Groups, "system:authenticated")
		assert.Empty(t, body.OrganizationID, "k8s identities carry no organization")
	})

	t.Run("invalid token is a 401", func(t *testing.T) {
		router, _ := newK8sRouter(t, nil, &authv1.TokenReviewStatus{Authenticated: false})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer bad-token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("authenticated token without system:authenticated is a 401", func(t *testing.T) {
		review := authenticatedReview()
		review.User.Groups = []string{"some-group"}
		router, _ := newK8sRouter(t, nil, review)

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("TokenReview API failure is a 503", func(t *testing.T) {
		router, _ := newK8sRouter(t, errors.New("apiserver unreachable"), nil)

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Run("inference API key bearer is rejected", func(t *testing.T) {
		router, client := newK8sRouter(t, nil, authenticatedReview())

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer "+constant.APIKeyPrefix+"secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, client.Actions(), "TokenReview must not be called for inference credentials")
	})

	t.Run("X-Api-Key credential is rejected", func(t *testing.T) {
		router, client := newK8sRouter(t, nil, authenticatedReview())

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Api-Key", constant.APIKeyPrefix+"secret")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, client.Actions(), "TokenReview must not be called for inference credentials")
	})

	t.Run("missing credentials are a 401 on strict routes", func(t *testing.T) {
		router, client := newK8sRouter(t, nil, authenticatedReview())

		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, client.Actions())
	})

	t.Run("gateway headers win over bearer token", func(t *testing.T) {
		router, client := newK8sRouter(t, nil, authenticatedReview())

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer sha256~oc-token")
		req.Header.Set(constant.HeaderUsername, "gateway-user")
		req.Header.Set(constant.HeaderGroup, `["ai-grid-users"]`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code)
		var body token.UserContext
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "gateway-user", body.Username)
		assert.Equal(t, token.AuthSourceGateway, body.AuthSource)
		assert.Empty(t, client.Actions(), "TokenReview must not run when headers select the gateway mechanism")
	})
}

func TestExtractUserInfo_KubernetesFallbackDisabled(t *testing.T) {
	// With the fallback disabled the historical behavior is preserved:
	// missing identity headers are a configuration error, never a TokenReview.
	gin.SetMode(gin.TestMode)
	h := token.NewHandler(logger.Development(), "test-tenant")
	router := gin.New()
	router.Use(h.ExtractUserInfo(token.AuthModeGateway))
	router.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer sha256~oc-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestExtractUserInfo_KubernetesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: *authenticatedReview()}, nil
	})
	handler := token.NewHandler(logger.Development(), "test-tenant").WithKubeClient(client)
	router := gin.New()
	router.Use(handler.ExtractUserInfo(token.AuthModeKubernetes))
	router.GET("/test", func(c *gin.Context) {
		user, _ := c.Get("user")
		c.JSON(http.StatusOK, user)
	})

	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set("Authorization", "Bearer sha256~oc-token")
	request.Header.Set(constant.HeaderUsername, "spoofed-gateway-user")
	request.Header.Set(constant.HeaderGroup, `["cluster-admins"]`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var user token.UserContext
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &user))
	assert.Equal(t, "kube-user", user.Username)
	assert.Equal(t, token.AuthSourceK8s, user.AuthSource)
	assert.Len(t, client.Actions(), 1, "TokenReview must authenticate Kubernetes-only routes")
}
