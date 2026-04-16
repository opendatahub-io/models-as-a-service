package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// RequestIDHeader is the HTTP header name for request IDs.
	RequestIDHeader = "X-Request-ID"
	// RequestIDKey is the context key for storing request IDs.
	RequestIDKey = "request_id"
)

// RequestID middleware generates or extracts a request ID and adds it to the context.
// If the request already has an X-Request-ID header (e.g., from the gateway),
// it uses that value. Otherwise, it generates a new UUID.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if request ID already exists (from gateway)
		requestID := c.GetHeader(RequestIDHeader)

		// If not present, generate a new one
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Store in context for handlers
		c.Set(RequestIDKey, requestID)

		// Add to response headers for client correlation
		c.Header(RequestIDHeader, requestID)

		c.Next()
	}
}

// GetRequestID retrieves the request ID from the gin context.
// Returns empty string if not found.
func GetRequestID(c *gin.Context) string {
	if requestID, exists := c.Get(RequestIDKey); exists {
		if id, ok := requestID.(string); ok {
			return id
		}
	}
	return ""
}
