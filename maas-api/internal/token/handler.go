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
	tenantName string
	logger     *logger.Logger
}

func NewHandler(log *logger.Logger, tenantName string) *Handler {
	if log == nil {
		log = logger.Production()
	}
	return &Handler{
		tenantName: tenantName,
		logger:     log,
	}
}

// ParseGroupsHeader parses the group header which may arrive as a JSON array
// (e.g., ["ai-eng"]) or as an Authorino bracket-wrapped format (e.g., [ai-eng]).
func ParseGroupsHeader(header string) ([]string, error) {
	if header == "" {
		return nil, errors.New("header is empty")
	}

	var parsedGroups []string

	// Try JSON array first (e.g., ["ai-eng", "platform"])
	if err := json.Unmarshal([]byte(header), &parsedGroups); err != nil {
		// Fall back to bracket-wrapped space-separated format (e.g., [ai-eng platform]).
		// Authorino's plain selector serializes arrays this way.
		trimmed := strings.TrimSpace(header)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inner := trimmed[1 : len(trimmed)-1]
			parsedGroups = strings.Fields(inner)
		} else {
			return nil, fmt.Errorf("unsupported group header format: %s", header)
		}
	}

	var validGroups []string
	for _, g := range parsedGroups {
		if tg := strings.TrimSpace(g); tg != "" {
			validGroups = append(validGroups, tg)
		}
	}

	if len(validGroups) == 0 {
		return nil, errors.New("no groups found in header")
	}

	return validGroups, nil
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
				"username", logger.RedactValue(username),
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
		groups, err := ParseGroupsHeader(groupHeader)
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

		// Create UserContext from headers and handler config.
		// Tenant comes from TENANT_NAME env var set during maas-api deployment.
		userContext := &UserContext{
			Username: username,
			Groups:   groups,
			Tenant:   h.tenantName,
		}

		h.logger.Debug("Extracted user info from headers",
			"username", logger.RedactValue(username),
			"groups", groups,
		)

		c.Set("user", userContext)
		c.Next()
	}
}

// ExtractUserInfoOptional is a lenient variant of ExtractUserInfo for endpoints
// that must remain accessible when no auth policy is active (e.g. no
// LLMInferenceService deployed). When the identity headers are entirely absent
// the middleware continues without setting a user context, letting the handler
// decide what to return. If headers ARE present but malformed, it still aborts
// so that real configuration errors are surfaced.
func (h *Handler) ExtractUserInfoOptional() gin.HandlerFunc {
	return func(c *gin.Context) {
		username := strings.TrimSpace(c.GetHeader(constant.HeaderUsername))
		groupHeader := c.GetHeader(constant.HeaderGroup)

		// When both identity headers are absent, continue without user context.
		// This allows the handler to return a graceful response (e.g. empty list).
		if username == "" && groupHeader == "" {
			h.logger.Debug("Auth identity headers not present, continuing without user context")
			c.Next()
			return
		}

		// If only one header is present, that is a partial / broken auth config.
		if username == "" {
			h.logger.Error("Missing or empty username header while group header is present",
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
			h.logger.Error("Missing group header while username header is present",
				"header", constant.HeaderGroup,
				"username", logger.RedactValue(username),
			)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":         "Exception thrown while generating token",
				"exceptionCode": "AUTH_FAILURE",
				"refId":         "002",
			})
			c.Abort()
			return
		}

		// Parse groups — malformed headers are still an error.
		groups, err := ParseGroupsHeader(groupHeader)
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

		userContext := &UserContext{
			Username: username,
			Groups:   groups,
			Tenant:   h.tenantName,
		}

		h.logger.Debug("Extracted user info from headers",
			"username", logger.RedactValue(username),
			"groups", groups,
		)

		c.Set("user", userContext)
		c.Next()
	}
}
