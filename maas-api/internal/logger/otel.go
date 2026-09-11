package logger

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const defaultServiceName = "maas-api"

// Format selects the log output format.
type Format string

const (
	// FormatZap preserves the existing zap output.
	FormatZap Format = "zap"
	// FormatOTelJSON emits logs using the OpenTelemetry Logs Data Model.
	FormatOTelJSON Format = "otel-json"
)

// ParseFormat validates a log format. The empty value preserves legacy output.
func ParseFormat(value string) (Format, error) {
	switch Format(value) {
	case "", FormatZap:
		return FormatZap, nil
	case FormatOTelJSON:
		return FormatOTelJSON, nil
	default:
		return "", fmt.Errorf("unsupported log format %q", value)
	}
}

// EncoderConfig returns a zap encoder config that emits OTel Logs Data Model
// field names on stdout JSON records.
func EncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "severity_text",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "body",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    EncodeSeverityText,
		EncodeTime:     EncodeTime,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// EncodeTime emits RFC 3339 timestamps in UTC.
func EncodeTime(value time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(value.UTC().Format(time.RFC3339Nano))
}

// EncodeSeverityText maps zap levels to OTel severity_text values.
func EncodeSeverityText(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(SeverityText(l))
}

// SeverityText returns the OTel severity_text for a zap level.
func SeverityText(l zapcore.Level) string {
	switch {
	case l >= zapcore.FatalLevel:
		return "FATAL"
	case l >= zapcore.PanicLevel:
		return "PANIC"
	case l >= zapcore.DPanicLevel:
		return "DPANIC"
	case l >= zapcore.ErrorLevel:
		return "ERROR"
	case l >= zapcore.WarnLevel:
		return "WARN"
	case l >= zapcore.InfoLevel:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// SeverityNumber returns the OTel severity_number for a zap level.
func SeverityNumber(l zapcore.Level) int {
	switch {
	case l >= zapcore.FatalLevel:
		return 21
	case l >= zapcore.PanicLevel:
		return 19
	case l >= zapcore.DPanicLevel:
		return 18
	case l >= zapcore.ErrorLevel:
		return 17
	case l >= zapcore.WarnLevel:
		return 13
	case l >= zapcore.InfoLevel:
		return 9
	default:
		return 5
	}
}

// ServiceName follows the OTel SDK environment precedence or returns fallback.
func ServiceName(fallback string) string {
	if value := os.Getenv("OTEL_SERVICE_NAME"); value != "" {
		return value
	}
	for item := range strings.SplitSeq(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		decodedKey, keyErr := url.PathUnescape(strings.TrimSpace(key))
		decodedValue, valueErr := url.PathUnescape(strings.TrimSpace(value))
		if keyErr == nil && valueErr == nil && decodedKey == "service.name" && decodedValue != "" {
			return decodedValue
		}
	}
	return fallback
}

// WrapCore adds severity_number to every log record.
func WrapCore(c zapcore.Core) zapcore.Core { //nolint:ireturn // zap.WrapCore requires returning zapcore.Core.
	return &otelCore{Core: c}
}

type otelCore struct {
	zapcore.Core
}

func (c *otelCore) With(fields []zapcore.Field) zapcore.Core { //nolint:ireturn // zapcore.Core.With returns zapcore.Core.
	return &otelCore{Core: c.Core.With(fields)}
}

func (c *otelCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Core.Check(ent, nil) != nil {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *otelCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	fields = append(fields, zap.Int("severity_number", SeverityNumber(ent.Level)))
	return c.Core.Write(ent, fields)
}

// TraceFields returns trace_id and span_id zap fields when a span is active on ctx.
func TraceFields(ctx context.Context) []any {
	if ctx == nil {
		return nil
	}
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return nil
	}
	return []any{
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	}
}
