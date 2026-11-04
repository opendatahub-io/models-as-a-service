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

// WithDatabaseHealth attaches optional database component status.
//
// The process remains healthy when an optional read replica is degraded. Failing
// readiness would remove every maas-api pods during replica maintenance even though
// writes and authorization can still be served through the writable primary.
func (h *HealthHandler) WithDatabaseHealth(reporter DatabaseHealthReporter) *HealthHandler {
	h.database = reporter
	return h
}

// HealthCheck handles GET /health.
func (h *HealthHandler) HealthCheck(c *gin.Context) {
	response := gin.H{"status": "healthy"}
	if h.database != nil {
		response["database"] = h.database.DatabaseHealth()
	}
	c.JSON(http.StatusOK, response)
}
