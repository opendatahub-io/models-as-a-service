package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func TestCleanupAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const expectedUsername = "system:serviceaccount:maas-infra:maas-api-cleanup"

	tests := []struct {
		name           string
		header         string
		authenticated  bool
		reviewUsername string
		wantStatus     int
	}{
		{name: "missing token", wantStatus: http.StatusUnauthorized},
		{name: "invalid token", header: "Bearer invalid", wantStatus: http.StatusUnauthorized},
		{name: "wrong service account", header: "Bearer valid", authenticated: true, reviewUsername: "system:serviceaccount:maas-infra:other", wantStatus: http.StatusForbidden},
		{name: "expected service account", header: "Bearer valid", authenticated: true, reviewUsername: expectedUsername, wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.TokenReview{
					Status: authv1.TokenReviewStatus{
						Authenticated: tt.authenticated,
						User:          authv1.UserInfo{Username: tt.reviewUsername},
					},
				}, nil
			})

			router := gin.New()
			router.Use(auth.CleanupAuthMiddleware(logger.Development(), client, "maas-api-cleanup", "maas-infra"))
			router.GET("/cleanup", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			req := httptest.NewRequest(http.MethodGet, "/cleanup", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.Code, tt.wantStatus)
			}
		})
	}
}

func TestCleanupAuthMiddlewareRejectsServiceAccountUsernameWithExtraText(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "system:serviceaccount:maas-infra:maas-api-cleanup:extra"},
			},
		}, nil
	})

	router := gin.New()
	router.Use(auth.CleanupAuthMiddleware(logger.Development(), client, "maas-api-cleanup", "maas-infra"))
	router.GET("/cleanup", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/cleanup", nil)
	req.Header.Set("Authorization", "Bearer valid")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusForbidden)
	}
}
