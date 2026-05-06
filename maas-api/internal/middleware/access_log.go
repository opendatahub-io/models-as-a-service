package middleware

import (
	"fmt"
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

	summary := logger.SensitiveHeadersSummaryForAccessLog(param.Request.Header)

	line := fmt.Sprintf("[GIN] %v |%s %3d %s| %13v | %15s |%s %-7s %s %#v\n%s",
		param.TimeStamp.Format("2006/01/02 - 15:04:05"),
		statusColor, param.StatusCode, resetColor,
		param.Latency,
		param.ClientIP,
		methodColor, param.Method, resetColor,
		param.Path,
		param.ErrorMessage,
	)
	suffix := " | " + summary + "\n"
	base, hadTrailingNL := strings.CutSuffix(line, "\n")
	if hadTrailingNL {
		return base + suffix
	}
	return line + suffix
}
