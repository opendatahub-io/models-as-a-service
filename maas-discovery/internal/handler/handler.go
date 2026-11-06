// Package handler provides HTTP handlers for the discovery service.
package handler

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

// Handler serves tenant discovery endpoints.
type Handler struct {
	cache cache.TenantCache
	log   *slog.Logger
}

// New creates a Handler backed by the given TenantCache.
func New(tc cache.TenantCache) *Handler {
	return NewWithLogger(tc, nil)
}

// NewWithLogger creates a Handler backed by the given TenantCache and logger.
func NewWithLogger(tc cache.TenantCache, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cache: tc, log: log}
}

// ListTenants handles GET /v1/tenants.
// The TokenReviewMiddleware must run before this handler to populate the
// auth.ContextKeyUsername, auth.ContextKeyGroups, and auth.ContextKeyIsAdmin values.
func (h *Handler) ListTenants(c *gin.Context) {
	username, _ := c.Get(auth.ContextKeyUsername)
	groupsRaw, _ := c.Get(auth.ContextKeyGroups)
	isAdminRaw, _ := c.Get(auth.ContextKeyIsAdmin)

	user, _ := username.(string)
	groups, _ := groupsRaw.([]string)
	isAdmin, _ := isAdminRaw.(bool)

	var tenants []types.TenantInfo
	if isAdmin {
		tenants = h.cache.List()
	} else {
		tenants = h.cache.ListForSubjects(user, groups)
	}
	if tenants == nil {
		tenants = []types.TenantInfo{}
	}
	h.log.Debug("resolved visible tenants",
		"username", user,
		"isAdmin", isAdmin,
		"groups_count", len(groups),
		"tenant_count", len(tenants),
	)
	c.JSON(http.StatusOK, types.TenantsResponse{Tenants: tenants})
}

// Healthz handles GET /healthz.
func (h *Handler) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Readyz handles GET /readyz. Returns 503 until the cache has completed
// its initial sync, then 200.
func (h *Handler) Readyz(c *gin.Context) {
	if !h.cache.Synced() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready", "reason": "cache not synced"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RegisterRoutes wires up all routes on the given engine.
// authMiddleware is applied to authenticated endpoints (/v1/tenants).
func (h *Handler) RegisterRoutes(r *gin.Engine, authMiddleware ...gin.HandlerFunc) {
	handlers := append(authMiddleware, h.ListTenants) //nolint:gocritic // intentional append to variadic copy
	r.GET("/v1/tenants", handlers...)
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
}
