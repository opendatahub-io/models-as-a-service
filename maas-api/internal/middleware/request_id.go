package middleware

import (
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// RequestIDHeader is the HTTP header name for request IDs.
	RequestIDHeader = "X-Request-ID"
	// RequestIDKey is the context key for storing request IDs.
	RequestIDKey = "request_id"
	// maxRequestIDLength is the maximum allowed length for request IDs.
	maxRequestIDLength = 128
)

var (
	// validRequestIDPattern allows only safe characters: alphanumeric, dot, underscore, hyphen.
	validRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// isValidRequestID checks if a request ID is safe for logging.
// Returns true if the ID contains only safe characters and is within length limits.
func isValidRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	return validRequestIDPattern.MatchString(id)
}

// RequestID middleware generates or extracts a request ID and adds it to the context.
// If the request has a valid X-Request-ID header (e.g., from the gateway),
// it uses that value. Otherwise, it generates a new UUID.
// Client-supplied values are validated to prevent log injection attacks.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if request ID already exists (from gateway or client)
		requestID := c.GetHeader(RequestIDHeader)

		// Validate client-supplied request ID to prevent log injection
		if requestID != "" && !isValidRequestID(requestID) {
			// Reject invalid request ID, generate a new one
			requestID = uuid.New().String()
		} else if requestID == "" {
			// Generate new UUID if not present
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
