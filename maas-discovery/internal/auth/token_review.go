// Package auth provides authentication middleware for the discovery service.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	kubeAPITimeout = 5 * time.Second

	// ContextKeyUsername is the gin context key for the authenticated username.
	ContextKeyUsername = "auth.username"
	// ContextKeyGroups is the gin context key for the authenticated user's groups.
	ContextKeyGroups = "auth.groups"
	// ContextKeyIsAdmin is the gin context key indicating caller admin visibility.
	ContextKeyIsAdmin = "auth.isAdmin"
)

// TokenReviewMiddleware validates bearer tokens via Kubernetes TokenReview and
// verifies the caller is authenticated via SubjectAccessReview. It also checks
// whether the caller can list AITenants in aitenantNamespace and marks those
// callers as admin-visible for tenant discovery.
func TokenReviewMiddleware(log *slog.Logger, kubeClient kubernetes.Interface, aitenantNamespace string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), kubeAPITimeout)
		defer cancel()

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			log.Debug("missing or invalid Authorization header")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication required",
				"details": "Missing or invalid bearer token",
			})
			c.Abort()
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")

		tr := &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}
		result, err := kubeClient.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
		if err != nil {
			log.Error("TokenReview failed", "error", err)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication failed",
				"details": "Token validation error",
			})
			c.Abort()
			return
		}

		if !result.Status.Authenticated {
			log.Debug("token not authenticated", "error", result.Status.Error)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication failed",
				"details": "Invalid token",
			})
			c.Abort()
			return
		}

		username := result.Status.User.Username
		groups := result.Status.User.Groups

		sar := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   username,
				Groups: groups,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group:    "authorization.k8s.io",
					Resource: "selfsubjectaccessreviews",
					Verb:     "create",
				},
			},
		}

		sarResult, err := kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
		if err != nil {
			log.Error("SubjectAccessReview failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "Authorization check failed",
				"details": "Unable to verify authenticated user status",
			})
			c.Abort()
			return
		}

		if !sarResult.Status.Allowed {
			log.Debug("access denied - not an authenticated user", "reason", sarResult.Status.Reason)
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "Insufficient permissions",
				"details": "User is not authorized to access cluster resources",
			})
			c.Abort()
			return
		}

		isAdmin := false
		if aitenantNamespace != "" {
			adminSAR := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:   username,
					Groups: groups,
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: aitenantNamespace,
						Verb:      "list",
						Group:     "maas.opendatahub.io",
						Resource:  "aitenants",
					},
				},
			}

			adminResult, adminErr := kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, adminSAR, metav1.CreateOptions{})
			if adminErr != nil {
				log.Error("admin SubjectAccessReview failed", "error", adminErr)
			} else {
				isAdmin = adminResult.Status.Allowed
			}
		}

		log.Debug("authenticated user access granted", "username", username, "isAdmin", isAdmin)

		c.Set(ContextKeyUsername, username)
		c.Set(ContextKeyGroups, groups)
		c.Set(ContextKeyIsAdmin, isAdmin)
		c.Next()
	}
}
