package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HealthHandler handles health check endpoints.
type HealthHandler struct {
	database DatabaseHealthReporter
}

// DatabaseHealthReporter exposes non-secret database pool status. A degraded
// optional component must not automatically make the process health check fail.
type DatabaseHealthReporter interface {
	DatabaseHealth() map[string]any
}

// NewHealthHandler creates a new health handler.
func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

// WithDatabaseHealth attaches optional database component status to the
// dedicated component health endpoint.
//
// The process remains healthy when an optional read replica is degraded. Failing
// readiness would remove every maas-api pods during replica maintenance even though
// writes and authorization can still be served through the writable primary.
func (h *HealthHandler) WithDatabaseHealth(reporter DatabaseHealthReporter) *HealthHandler {
	h.database = reporter
	return h
}

// HealthCheck handles GET /health.
//
// Keep this public process-health contract stable and implementation-agnostic:
// infrastructure probes and clients must not start depending on deployment
// details such as whether a read replica is configured.
func (h *HealthHandler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "healthy"})
}

// DatabaseHealthCheck handles GET /health/database.
//
// This is a component diagnostics endpoint, not the process readiness or
// liveness check. Separating it lets operators inspect non-secret database
// pool state without changing the public /health response shape or failing
// the process when an optional read replica is degraded.
func (h *HealthHandler) DatabaseHealthCheck(c *gin.Context) {
	response := gin.H{"status": "healthy"}
	if h.database != nil {
		response["database"] = h.database.DatabaseHealth()
	}
	c.JSON(http.StatusOK, response)
}
