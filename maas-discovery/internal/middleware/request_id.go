package middleware

import (
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// RequestIDHeader is the HTTP header name for request IDs.
	RequestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
	maxRequestIDLen = 128
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RequestID extracts or generates a request ID and stores it in the gin context.
// Client-supplied values are validated to prevent log injection.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" || len(id) > maxRequestIDLen || !validRequestID.MatchString(id) {
			id = uuid.New().String()
		}

		c.Set(requestIDKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// GetRequestID retrieves the request ID from the gin context.
func GetRequestID(c *gin.Context) string {
	if id, ok := c.Get(requestIDKey); ok {
		if s, ok := id.(string); ok {
			return s
		}
	}
	return ""
}
