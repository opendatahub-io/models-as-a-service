package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const loggerKey = "logger"

// TenantLoggerConfig holds static tenant context injected into per-request loggers.
type TenantLoggerConfig struct {
	DefaultTenant   string
	TenantNamespace string
	GatewayName     string
}

// TenantLogger returns a Gin middleware that creates a per-request logger
// enriched with tenant context and request ID. The enriched logger is stored
// in the Gin context and accessible via GetLogger.
func TenantLogger(base *logger.Logger, cfg TenantLoggerConfig) gin.HandlerFunc {
	if base == nil {
		base = logger.Production()
	}
	return func(c *gin.Context) {
		tenantName := cfg.DefaultTenant
		if u, ok := c.Get("user"); ok {
			if uc, ok := u.(*token.UserContext); ok {
				tenantName = uc.Tenant
			}
		}

		fields := []any{
			"tenant_name", tenantName,
			"tenant_namespace", cfg.TenantNamespace,
			"gateway_name", cfg.GatewayName,
		}

		if requestID := GetRequestID(c); requestID != "" {
			fields = append(fields, "request_id", requestID)
		}

		enriched := base.WithFields(fields...)
		c.Set(loggerKey, enriched)
		c.Next()
	}
}

// GetLogger retrieves the per-request logger from the Gin context.
// Returns nil if no logger was set by TenantLogger middleware.
func GetLogger(c *gin.Context) *logger.Logger {
	if l, ok := c.Get(loggerKey); ok {
		if log, ok := l.(*logger.Logger); ok {
			return log
		}
	}
	return nil
}
