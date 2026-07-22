package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const maxRequestBodyBytes int64 = 1 << 20 // 1 MiB

// BodyLimit returns middleware that limits request body size to 1 MiB.
// Requests exceeding the limit receive 413 Request Entity Too Large.
// This prevents a single authenticated caller from OOM-killing the pod
// by sending an oversized JSON payload to endpoints that use ShouldBindJSON.
func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
		}
		c.Next()
	}
}
