package middleware

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/metrics"
)

// Prometheus instruments each request with the discovery service metrics:
// request counter and latency histogram, including time spent in the auth
// middleware (a cache miss costs a TokenReview plus SARs, so total request
// time is the latency that matters). Probe paths (/healthz, /readyz) are
// excluded to keep the signal clean; requests to unmatched paths (no gin
// route) are ignored.
func Prometheus(m *metrics.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" || route == "/healthz" || route == "/readyz" {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()
		m.ObserveRequest(c.Writer.Status(), route, time.Since(start))
	}
}
