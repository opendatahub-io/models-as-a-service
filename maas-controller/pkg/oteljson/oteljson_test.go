package oteljson_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

func TestBindFlags(t *testing.T) {
	t.Parallel()

	var format oteljson.Format
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	oteljson.BindFlags(fs, &format)
	require.Equal(t, oteljson.FormatZap, format)
	require.NoError(t, fs.Parse([]string{"--log-format=otel-json"}))
	assert.Equal(t, oteljson.FormatOTelJSON, format)
}

func TestBindFlagsRejectsUnknownFormat(t *testing.T) {
	t.Parallel()

	var format oteljson.Format
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	oteljson.BindFlags(fs, &format)
	assert.Error(t, fs.Parse([]string{"--log-format=unknown"}))
}

func TestSeverityMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		level  zapcore.Level
		text   string
		number int
	}{
		{name: "logr V(5)", level: -5, text: "DEBUG", number: 5},
		{name: "logr V(4)", level: -4, text: "DEBUG", number: 5},
		{name: "logr V(3)", level: -3, text: "DEBUG", number: 5},
		{name: "logr V(1)", level: -1, text: "DEBUG", number: 5},
		{name: "info", level: zapcore.InfoLevel, text: "INFO", number: 9},
		{name: "warn", level: zapcore.WarnLevel, text: "WARN", number: 13},
		{name: "error", level: zapcore.ErrorLevel, text: "ERROR", number: 17},
		{name: "dpanic", level: zapcore.DPanicLevel, text: "DPANIC", number: 18},
		{name: "panic", level: zapcore.PanicLevel, text: "PANIC", number: 19},
		{name: "fatal", level: zapcore.FatalLevel, text: "FATAL", number: 21},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.text, oteljson.SeverityText(tt.level))
			assert.Equal(t, tt.number, oteljson.SeverityNumber(tt.level))
		})
	}
}

func TestJSONRecordUsesOTelFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	encCfg := zap.NewProductionEncoderConfig()
	oteljson.ConfigureEncoder(&encCfg)
	enc := zapcore.NewJSONEncoder(encCfg)
	core := oteljson.WrapCore(zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.InfoLevel))
	zl := zap.New(core)
	zl.Info("controller started")
	require.NoError(t, zl.Sync())

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "controller started", rec["body"])
	assert.Equal(t, "INFO", rec["severity_text"])
	assert.InDelta(t, float64(9), rec["severity_number"], 0)
	assert.NotEmpty(t, rec["timestamp"])
}

func TestApplyForcesJSONWhenDevelopmentEncoderWasSelected(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "maas-controller")

	var buf bytes.Buffer
	opts := crzap.Options{Development: true, DestWriter: &buf}
	oteljson.Apply(&opts, "fallback")
	logger := crzap.NewRaw(crzap.UseFlagOptions(&opts))
	logger.Info("controller started")
	require.NoError(t, logger.Sync())

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "controller started", rec["body"])
	assert.Equal(t, "INFO", rec["severity_text"])
}

func TestTimestampIsUTC(t *testing.T) {
	t.Parallel()

	encCfg := zap.NewProductionEncoderConfig()
	oteljson.ConfigureEncoder(&encCfg)
	enc := zapcore.NewJSONEncoder(encCfg)
	entry := zapcore.Entry{
		Time:    time.Date(2026, time.September, 11, 12, 34, 56, 123000000, time.FixedZone("test", 3600)),
		Level:   zapcore.InfoLevel,
		Message: "controller started",
	}
	encoded, err := enc.EncodeEntry(entry, nil)
	require.NoError(t, err)

	var rec map[string]any
	require.NoError(t, json.Unmarshal(encoded.Bytes(), &rec))
	assert.Equal(t, "2026-09-11T11:34:56.123Z", rec["timestamp"])
}

func TestWrapCorePreservesSamplingDecisions(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zapcore.InfoLevel)
	core = zapcore.NewSamplerWithOptions(core, time.Minute, 1, 0)
	logger := zap.New(oteljson.WrapCore(core))

	logger.Info("repeated")
	logger.Info("repeated")

	assert.Len(t, strings.Split(strings.TrimSpace(buf.String()), "\n"), 1)
}

func TestServiceNameUsesOTelEnvironmentPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		service    string
		attributes string
		want       string
	}{
		{name: "fallback", want: "default"},
		{name: "resource attributes", attributes: "service.name=from-attributes", want: "from-attributes"},
		{name: "service name wins", service: "from-service-name", attributes: "service.name=from-attributes", want: "from-service-name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_SERVICE_NAME", tt.service)
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.attributes)
			assert.Equal(t, tt.want, oteljson.ServiceName("default"))
		})
	}
}

func TestFromContextInjectsTraceIDs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	encCfg := zap.NewProductionEncoderConfig()
	oteljson.ConfigureEncoder(&encCfg)
	zl := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(&buf), zapcore.InfoLevel))

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	ctx, span := tp.Tracer("test").Start(t.Context(), "reconcile")
	defer span.End()
	ctx = log.IntoContext(ctx, zapr.NewLogger(zl))

	oteljson.FromContext(ctx).Info("reconciling")
	require.NoError(t, zl.Sync())

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	sc := span.SpanContext()
	assert.Equal(t, sc.TraceID().String(), rec["trace_id"])
	assert.Equal(t, sc.SpanID().String(), rec["span_id"])
}

func TestIntoContextInjectsTraceIDsOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	encCfg := zap.NewProductionEncoderConfig()
	oteljson.ConfigureEncoder(&encCfg)
	zl := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(&buf), zapcore.InfoLevel))

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	ctx, span := tp.Tracer("test").Start(t.Context(), "reconcile")
	defer span.End()
	ctx = log.IntoContext(ctx, zapr.NewLogger(zl))
	ctx = oteljson.IntoContext(ctx)
	ctx = oteljson.IntoContext(ctx)

	oteljson.FromContext(ctx).Info("reconciling")
	require.NoError(t, zl.Sync())
	assert.Equal(t, 1, strings.Count(buf.String(), `"trace_id"`))
	assert.Equal(t, 1, strings.Count(buf.String(), `"span_id"`))
}
