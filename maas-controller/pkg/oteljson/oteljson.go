package oteljson

import (
	"context"
	"os"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const defaultServiceName = "maas-controller"

// Apply configures controller-runtime zap options for OTel JSON stdout logs.
func Apply(opts *crzap.Options, serviceName string) {
	if opts == nil {
		return
	}
	opts.DestWriter = os.Stdout
	opts.EncoderConfigOptions = append(opts.EncoderConfigOptions, func(ec *zapcore.EncoderConfig) {
		ConfigureEncoder(ec)
	})
	opts.ZapOpts = append(opts.ZapOpts,
		zap.WrapCore(WrapCore),
		zap.Fields(zap.String("service.name", ServiceName(serviceName))),
	)
}

// ConfigureEncoder sets OTel Logs Data Model JSON field names.
func ConfigureEncoder(ec *zapcore.EncoderConfig) {
	ec.TimeKey = "timestamp"
	ec.LevelKey = "severity_text"
	ec.NameKey = "logger"
	ec.CallerKey = "caller"
	ec.MessageKey = "body"
	ec.StacktraceKey = "stacktrace"
	ec.EncodeLevel = EncodeSeverityText
	ec.EncodeTime = zapcore.RFC3339NanoTimeEncoder
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
	if fallback != "" {
		return fallback
	}
	return defaultServiceName
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

// FromContext returns the logr logger from ctx with trace_id/span_id when a span is active.
func FromContext(ctx context.Context) logr.Logger {
	logger := log.FromContext(ctx)
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return logger
	}
	return logger.WithValues("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}
