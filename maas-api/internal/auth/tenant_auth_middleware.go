package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	authenticationv1 "k8s.io/api/authentication/v1" //nolint:importas // Must use distinct alias from authorization/v1
	authorizationv1 "k8s.io/api/authorization/v1"   //nolint:importas // Must use distinct alias from authentication/v1
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// TenantAuthMiddleware validates bearer tokens via TokenReview and checks tenant access via SubjectAccessReview.
// It verifies that the caller is a member of the system:authenticated group, which includes all authenticated users.
func TenantAuthMiddleware(log *logger.Logger, kubeClient kubernetes.Interface) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Set a hard timeout for all Kubernetes API calls to prevent hangs
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()

		// Step 1: Extract and validate bearer token
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			log.Debug("Missing or invalid Authorization header")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication required",
				"details": "Missing or invalid bearer token",
			})
			c.Abort()
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")

		// Step 2: Validate token via TokenReview and extract user identity
		tr := &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}
		result, err := kubeClient.AuthenticationV1().TokenReviews().Create(
			ctx, tr, metav1.CreateOptions{},
		)

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
			log.Debug("Token not authenticated",
				"error", result.Status.Error)
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication failed",
				"details": "Invalid token",
			})
			c.Abort()
			return
		}

		username := result.Status.User.Username
		groups := result.Status.User.Groups

		log.Debug("Token authenticated successfully")

		// Step 3: Verify user is in system:authenticated group via SubjectAccessReview
		// This ensures the user has basic authenticated access to the cluster.
		// We check this via SAR by verifying the user can perform a minimal operation
		// that system:authenticated users can do (selfsubjectaccessreviews).
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

		sarResult, err := kubeClient.AuthorizationV1().SubjectAccessReviews().Create(
			ctx, sar, metav1.CreateOptions{},
		)

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
			log.Debug("Access denied - not an authenticated user",
				"reason", sarResult.Status.Reason)
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "Insufficient permissions",
				"details": "User is not authorized to access cluster resources",
			})
			c.Abort()
			return
		}

		log.Debug("Authenticated user access granted")

		// Token valid and user is authenticated - proceed
		c.Next()
	}
}
