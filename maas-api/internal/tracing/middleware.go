package tracing

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const tracerName = "maas-api"

// NewMiddleware returns a Gin middleware that creates OTEL spans for each request
// and enriches them with tenant context from UserContext.
// If no TracerProvider is configured, spans are noops (zero overhead).
// defaultTenant is used when no UserContext is available (internal/health routes).
func NewMiddleware(defaultTenant, tenantNamespace, gatewayName, gatewayNamespace string) gin.HandlerFunc {
	tracer := otel.Tracer(tracerName)

	return func(c *gin.Context) {
		ctx := c.Request.Context()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		spanName := fmt.Sprintf("%s %s", c.Request.Method, route)

		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		defer func() {
			tenantName := defaultTenant
			if u, ok := c.Get("user"); ok {
				if uc, ok := u.(*token.UserContext); ok {
					tenantName = uc.Tenant
				}
			}

			status := c.Writer.Status()
			span.SetAttributes(
				attribute.String("http.method", c.Request.Method),
				attribute.String("http.route", route),
				attribute.Int("http.status_code", status),
				attribute.String("tenant.name", tenantName),
				attribute.String("tenant.namespace", tenantNamespace),
				attribute.String("gateway.name", gatewayName),
				attribute.String("gateway.namespace", gatewayNamespace),
			)

			if status >= 500 {
				span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(status))
			}
		}()

		c.Next()
	}
}
