package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func NewMiddleware(recorder MetricsRecorder) gin.HandlerFunc {
	if recorder == nil {
		panic("metrics.NewMiddleware: nil MetricsRecorder")
	}
	return func(c *gin.Context) {
		method := c.Request.Method
		recorder.IncrementInFlight(method)
		defer recorder.DecrementInFlight(method)
		start := time.Now()

		c.Next()
		duration := time.Since(start)

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}

		status := strconv.Itoa(c.Writer.Status())
		recorder.RecordRequestDuration(method, route, status, duration)
	}
}
