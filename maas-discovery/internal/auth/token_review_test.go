package auth_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/auth"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupRouter(kubeClient *kubefake.Clientset) *gin.Engine {
	log := slog.Default()
	r := gin.New()
	r.GET("/v1/tenants",
		auth.TokenReviewMiddleware(log, kubeClient, "ai-tenants"),
		func(c *gin.Context) {
			username, _ := c.Get(auth.ContextKeyUsername)
			groups, _ := c.Get(auth.ContextKeyGroups)
			isAdmin, _ := c.Get(auth.ContextKeyIsAdmin)
			c.JSON(http.StatusOK, gin.H{
				"username": username,
				"groups":   groups,
				"isAdmin":  isAdmin,
			})
		},
	)
	return r
}

func fakeAuthenticatedClient(username string, groups []string) *kubefake.Clientset {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User: authenticationv1.UserInfo{
					Username: username,
					Groups:   groups,
				},
			},
		}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})
	return client
}

func TestTokenReview_Success(t *testing.T) {
	client := fakeAuthenticatedClient("alice", []string{"system:authenticated", "team-a"})
	r := setupRouter(client)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "alice", body["username"])
	assert.Equal(t, true, body["isAdmin"])
}

func TestTokenReview_NonAdmin(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User: authenticationv1.UserInfo{
					Username: "bob",
					Groups:   []string{"system:authenticated", "team-b"},
				},
			},
		}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sar, ok := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		require.True(t, ok)
		if sar.Spec.ResourceAttributes != nil && sar.Spec.ResourceAttributes.Resource == "aitenants" {
			return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false}}, nil
		}
		return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	r := setupRouter(client)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "bob", body["username"])
	assert.Equal(t, false, body["isAdmin"])
}

func TestTokenReview_MissingAuthHeader(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	r := setupRouter(client)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "bearer token")
}

func TestTokenReview_InvalidToken(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{Authenticated: false, Error: "token expired"},
		}, nil
	})
	r := setupRouter(client)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer expired-token")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid token")
}

func TestTokenReview_SARDenied(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User:          authenticationv1.UserInfo{Username: "rogue"},
			},
		}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false, Reason: "not authorized"},
		}, nil
	})
	r := setupRouter(client)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer rogue-token")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "Insufficient permissions")
}
