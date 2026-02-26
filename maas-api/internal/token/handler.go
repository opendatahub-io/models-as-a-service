package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type Handler struct {
	name   string
	logger *logger.Logger
}

func NewHandler(log *logger.Logger, name string) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		name:   name,
		logger: log,
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
