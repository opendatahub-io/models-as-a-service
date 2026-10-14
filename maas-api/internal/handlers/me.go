package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// IdentityHandler handles /v1/me requests.
type IdentityHandler struct {
	log *logger.Logger
}

// NewIdentityHandler creates a new identity handler.
func NewIdentityHandler(log *logger.Logger) *IdentityHandler {
	return &IdentityHandler{log: log}
}

// MeResponse is the bounded response returned by GET /v1/me.
type MeResponse struct {
	Sub           string                         `json:"sub"`
	Email         string                         `json:"email,omitempty"`
	Name          string                         `json:"name,omitempty"`
	Organizations []token.OrganizationMembership `json:"https://redhat.com/maas/orgs"`
}

// GetMe returns the caller's verified identity.
// GET /v1/me.
func (h *IdentityHandler) GetMe(c *gin.Context) {
	handler := *h
	if requestLogger := middleware.GetLogger(c); requestLogger != nil {
		handler.log = requestLogger
	} else {
		handler.log = h.log.WithContext(c.Request.Context())
	}

	value, exists := c.Get("verified_identity")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Verified SSO identity is required",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	identity, ok := value.(*token.VerifiedIdentity)
	if !ok || identity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"message": "Verified SSO identity is required",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	organizations := identity.Claims.Organizations
	if organizations == nil {
		organizations = []token.OrganizationMembership{}
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, MeResponse{
		Sub:           identity.Claims.Subject,
		Email:         identity.Claims.Email,
		Name:          identity.Claims.Name,
		Organizations: organizations,
	})
}
