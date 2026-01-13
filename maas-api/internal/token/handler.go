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
func parseGroupsHeader(header string) ([]string, error) {
	if header == "" {
		return nil, errors.New("header is empty")
	}

	// Try to unmarshal as JSON array directly
	var groups []string
	if err := json.Unmarshal([]byte(header), &groups); err != nil {
		return nil, fmt.Errorf("failed to parse header as JSON array: %w", err)
	}

	if len(groups) == 0 {
		return nil, errors.New("no groups found in header")
	}

	// Trim whitespace from each group
	for i := range groups {
		groups[i] = strings.TrimSpace(groups[i])
	}

	return groups, nil
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

	// For ephemeral tokens, we explicitly pass an empty name.
	token, err := h.manager.GenerateToken(c.Request.Context(), user, expiration)
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
