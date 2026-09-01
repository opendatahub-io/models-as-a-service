package logger

import (
	"context"
	"os"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const defaultServiceName = "maas-api"

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
		EncodeTime:     zapcore.RFC3339NanoTimeEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// EncodeSeverityText maps zap levels to OTel severity_text values.
func EncodeSeverityText(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(SeverityText(l))
}

// SeverityText returns the OTel severity_text for a zap level.
func SeverityText(l zapcore.Level) string {
	switch {
	case l >= zapcore.DPanicLevel:
		return "FATAL"
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
	case l >= zapcore.DPanicLevel:
		return 21
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

// ServiceName returns OTEL_SERVICE_NAME or fallback.
func ServiceName(fallback string) string {
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		return name
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
	if c.Enabled(ent.Level) {
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
