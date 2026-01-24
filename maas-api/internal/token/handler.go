package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type Handler struct {
	name    string
	manager *Manager
	logger  *logger.Logger
}

func NewHandler(log *logger.Logger, name string, manager *Manager) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		name:    name,
		manager: manager,
		logger:  log,
	}
}

// parseGroupsHeader parses the group header which comes as a JSON array.
// Format: "[\"group1\",\"group2\",\"group3\"]" (JSON-encoded array string).
// Also handles Authorino format: "[group1 group2 group3]" (space-separated string representation).
func parseGroupsHeader(header string) ([]string, error) {
	if header == "" {
		return nil, errors.New("header is empty")
	}

	// First, try to unmarshal as JSON array directly (standard format)
	var groups []string
	if err := json.Unmarshal([]byte(header), &groups); err == nil {
		// Successfully parsed as JSON array
		if len(groups) == 0 {
			return nil, errors.New("no groups found in header")
		}
		// Trim whitespace from each group
		for i := range groups {
			groups[i] = strings.TrimSpace(groups[i])
		}
		return groups, nil
	}

	// If JSON parsing failed, try to handle Authorino format: "[group1 group2 group3]"
	// Remove brackets and split by space
	trimmed := strings.TrimSpace(header)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		// Remove brackets
		trimmed = trimmed[1 : len(trimmed)-1]
		// Split by space and filter empty strings
		parts := strings.Fields(trimmed)
		if len(parts) == 0 {
			return nil, errors.New("no groups found in header")
		}
		// Trim whitespace from each group
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, nil
	}

	// If neither format works, return a generic error
	return nil, fmt.Errorf("failed to parse header as JSON array or Authorino format: header=%q", header)
}

// ExtractUserInfo extracts user information from headers set by the auth policy.
func (h *Handler) ExtractUserInfo() gin.HandlerFunc {
	return func(c *gin.Context) {
		username := strings.TrimSpace(c.GetHeader(constant.HeaderUsername))
		groupHeader := c.GetHeader(constant.HeaderGroup)

		// Validate required headers exist and are not empty
		// Missing headers indicate a configuration issue with the auth policy (internal error)
		if username == "" {
			h.logger.Error("Missing or empty username header",
				"header", constant.HeaderUsername,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":         "Exception thrown while generating token",
				"exceptionCode": "AUTH_FAILURE",
				"refId":         "001",
			})
			c.Abort()
			return
		}

		if groupHeader == "" {
			h.logger.Error("Missing group header",
				"header", constant.HeaderGroup,
				"username", username,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":         "Exception thrown while generating token",
				"exceptionCode": "AUTH_FAILURE",
				"refId":         "002",
			})
			c.Abort()
			return
		}

		// Parse groups from header - format: "[group1 group2 group3]"
		// Parsing errors also indicate configuration issues
		groups, err := parseGroupsHeader(groupHeader)
		if err != nil {
			h.logger.Error("Failed to parse group header",
				"header", constant.HeaderGroup,
				"header_value", groupHeader,
				"error", err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":         "Exception thrown while generating token",
				"exceptionCode": "AUTH_FAILURE",
				"refId":         "003",
			})
			c.Abort()
			return
		}

		// Create UserContext from headers
		userContext := &UserContext{
			Username: username,
			Groups:   groups,
		}

		h.logger.Debug("Extracted user info from headers",
			"username", username,
			"groups", groups,
		)

		c.Set("user", userContext)
		c.Next()
	}
}

// IssueToken handles POST /v1/tokens for issuing ephemeral tokens.
func (h *Handler) IssueToken(c *gin.Context) {
	var req Request
	// BindJSON will still parse the request body, but we'll ignore the name field.
	if err := c.ShouldBindJSON(&req); err != nil {
		// Allow empty request body for default expiration
		if !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	if req.Expiration == nil {
		req.Expiration = &Duration{time.Hour * 4}
	}

	userCtx, exists := c.Get("user")
	if !exists {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "User context not found"})
		return
	}

	user, ok := userCtx.(*UserContext)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid user context type"})
		return
	}

	expiration := req.Expiration.Duration
	if err := ValidateExpiration(expiration, 10*time.Minute); err != nil {
		response := gin.H{"error": err.Error()}
		if expiration > 0 && expiration < 10*time.Minute {
			response["provided_expiration"] = expiration.String()
		}
		c.JSON(http.StatusBadRequest, response)
		return
	}

	// Extract OpenShift token from Authorization header for Keycloak exchange
	var openshiftToken string
	authHeader := c.GetHeader("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		openshiftToken = strings.TrimPrefix(authHeader, "Bearer ")
	}

	// For ephemeral tokens, we explicitly pass an empty name.
	// Pass OpenShift token if available (for Keycloak mode)
	token, err := h.manager.GenerateToken(c.Request.Context(), user, expiration, "", openshiftToken)
	if err != nil {
		h.logger.Error("Failed to generate token",
			"error", err,
			"expiration", expiration.String(),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	response := Response{
		Token: token,
	}

	c.JSON(http.StatusCreated, response)
}
