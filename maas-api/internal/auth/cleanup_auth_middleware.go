package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

const cleanupAuthTimeout = 5 * time.Second

// CleanupAuthMiddleware authenticates cleanup requests with a Kubernetes
// service-account token and only permits the configured cleanup ServiceAccount.
// NetworkPolicy remains defense in depth, but is not the authorization boundary.
func CleanupAuthMiddleware(log *logger.Logger, kubeClient kubernetes.Interface, serviceAccountName, namespace string) gin.HandlerFunc {
	if log == nil {
		log = logger.Production()
	}
	expectedUsername := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccountName)

	return func(c *gin.Context) {
		authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
		scheme, bearerToken, found := strings.Cut(authHeader, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(bearerToken) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "cleanup authentication required"})
			c.Abort()
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), cleanupAuthTimeout)
		defer cancel()
		review, err := kubeClient.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: strings.TrimSpace(bearerToken)},
		}, metav1.CreateOptions{})
		if err != nil {
			log.Error("Cleanup service-account TokenReview failed", "error", err)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "cleanup authentication failed"})
			c.Abort()
			return
		}
		if !review.Status.Authenticated {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "cleanup token is not authenticated"})
			c.Abort()
			return
		}
		if review.Status.User.Username != expectedUsername {
			log.Warn("Cleanup request rejected for unexpected service account", "username", logger.RedactValue(review.Status.User.Username))
			c.JSON(http.StatusForbidden, gin.H{"error": "cleanup service account is not authorized"})
			c.Abort()
			return
		}

		c.Next()
	}
}
