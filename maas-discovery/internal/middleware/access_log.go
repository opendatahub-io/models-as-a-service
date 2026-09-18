package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// AccessLogger produces a structured log entry for each request.
// Probe paths (/healthz, /readyz) are logged at DEBUG to reduce noise.
func AccessLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		path := c.Request.URL.Path
		level := slog.LevelInfo
		if path == "/healthz" || path == "/readyz" {
			level = slog.LevelDebug
		}

		log.Log(c.Request.Context(), level, "request completed",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"client_ip", c.ClientIP(),
			"request_id", GetRequestID(c),
		)
	}
}
