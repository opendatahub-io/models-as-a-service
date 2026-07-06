package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// AccessLogger is like gin.Logger() but appends a redacted sensitive-header summary.
func AccessLogger() gin.HandlerFunc {
	return gin.LoggerWithConfig(gin.LoggerConfig{
		Formatter: accessLogFormatter,
	})
}

func accessLogFormatter(param gin.LogFormatterParams) string {
	var statusColor, methodColor, resetColor string
	if param.IsOutputColor() {
		statusColor = param.StatusCodeColor()
		methodColor = param.MethodColor()
		resetColor = param.ResetColor()
	}

	if param.Latency > time.Minute {
		param.Latency = param.Latency.Truncate(time.Second)
	}

	line := fmt.Sprintf("[GIN] %v |%s %3d %s| %13v | %15s |%s %-7s %s %#v\n%s",
		param.TimeStamp.Format("2006/01/02 - 15:04:05"),
		statusColor, param.StatusCode, resetColor,
		param.Latency,
		param.ClientIP,
		methodColor, param.Method, resetColor,
		param.Path,
		param.ErrorMessage,
	)

	// Only append sensitive header summary if at least one is present
	// (avoids noise on health checks and other requests with no auth)
	if hasSensitiveHeaders(param.Request.Header) {
		summary := logger.SensitiveHeadersSummaryForAccessLog(param.Request.Header)
		suffix := " | " + summary + "\n"
		base, hadTrailingNL := strings.CutSuffix(line, "\n")
		if hadTrailingNL {
			return base + suffix
		}
		return line + suffix
	}

	return line
}

// hasSensitiveHeaders checks if any sensitive header has a non-empty value.
func hasSensitiveHeaders(h http.Header) bool {
	for _, name := range logger.SensitiveHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}
