// Package handler provides HTTP handlers for the discovery service.
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

const (
	usernameHeader = "X-MaaS-Username"
	groupHeader    = "X-MaaS-Group"
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
func (h *Handler) ListTenants(c *gin.Context) {
	username, groups, err := extractIdentity(c)
	if err != nil {
		h.log.Warn("invalid discovery identity headers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth identity headers are missing or invalid"})
		return
	}

	tenants := h.cache.ListForSubjects(username, groups)
	if tenants == nil {
		tenants = []types.TenantInfo{}
	}
	h.log.Debug("resolved visible tenants",
		"username_present", username != "",
		"groups_count", len(groups),
		"tenant_count", len(tenants),
	)
	c.JSON(http.StatusOK, types.TenantsResponse{Tenants: tenants})
}

func extractIdentity(c *gin.Context) (string, []string, error) {
	username := strings.TrimSpace(c.GetHeader(usernameHeader))
	rawGroups := c.GetHeader(groupHeader)
	if username == "" || rawGroups == "" {
		return "", nil, errors.New("missing identity headers")
	}

	groups, err := parseGroupsHeader(rawGroups)
	if err != nil {
		return "", nil, err
	}

	return username, groups, nil
}

func parseGroupsHeader(header string) ([]string, error) {
	if strings.TrimSpace(header) == "" {
		return nil, errors.New("header is empty")
	}

	var parsed []string
	if err := json.Unmarshal([]byte(header), &parsed); err != nil {
		trimmed := strings.TrimSpace(header)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			parsed = strings.Fields(trimmed[1 : len(trimmed)-1])
		} else {
			return nil, errors.New("unsupported group header format")
		}
	}

	groups := make([]string, 0, len(parsed))
	for _, g := range parsed {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return nil, errors.New("no groups found")
	}

	return groups, nil
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
