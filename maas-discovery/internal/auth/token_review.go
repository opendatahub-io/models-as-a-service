// Package auth provides authentication middleware for the discovery service.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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

// TokenReviewMiddleware authenticates the request's bearer token through the
// given Reviewer (typically a ReviewCache wrapping a TokenReviewer) and
// rejects unauthenticated or unauthorized callers before the handler runs.
func TokenReviewMiddleware(log *slog.Logger, reviewer Reviewer) gin.HandlerFunc {
	return func(c *gin.Context) {
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

		ctx, cancel := context.WithTimeout(c.Request.Context(), kubeAPITimeout)
		defer cancel()

		result, err := reviewer.Review(ctx, token)
		if err != nil {
			switch {
			case errors.Is(err, ErrTokenValidation):
				c.JSON(http.StatusUnauthorized, gin.H{
					"error":   "Authentication failed",
					"details": "Token validation error",
				})
			default:
				// ErrAuthorizationCheck and anything unexpected.
				log.Error("authorization check failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error":   "Authorization check failed",
					"details": "Unable to verify authenticated user status",
				})
			}
			c.Abort()
			return
		}

		if !result.Authenticated {
			log.Debug("token not authenticated")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "Authentication failed",
				"details": "Invalid token",
			})
			c.Abort()
			return
		}

		if !result.Authorized {
			log.Debug("access denied - not an authenticated user")
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "Insufficient permissions",
				"details": "User is not authorized to access cluster resources",
			})
			c.Abort()
			return
		}

		log.Debug("authenticated user access granted", "username", result.Username, "isAdmin", result.IsAdmin)

		c.Set(ContextKeyUsername, result.Username)
		c.Set(ContextKeyGroups, result.Groups)
		c.Set(ContextKeyIsAdmin, result.IsAdmin)
		c.Next()
	}
}
