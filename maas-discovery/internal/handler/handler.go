// Package handler provides HTTP handlers for the discovery service.
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

// Handler serves tenant discovery endpoints.
type Handler struct {
	cache cache.TenantCache
}

// New creates a Handler backed by the given TenantCache.
func New(tc cache.TenantCache) *Handler {
	return &Handler{cache: tc}
}

// ListTenants handles GET /v1/tenants.
func (h *Handler) ListTenants(c *gin.Context) {
	tenants := h.cache.List()
	if tenants == nil {
		tenants = []types.TenantInfo{}
	}
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
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/v1/tenants", h.ListTenants)
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
}
