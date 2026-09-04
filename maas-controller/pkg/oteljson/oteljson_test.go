package oteljson_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

func TestSeverityMapping(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "DEBUG", oteljson.SeverityText(zapcore.DebugLevel))
	assert.Equal(t, 5, oteljson.SeverityNumber(zapcore.DebugLevel))
	assert.Equal(t, "INFO", oteljson.SeverityText(zapcore.InfoLevel))
	assert.Equal(t, 9, oteljson.SeverityNumber(zapcore.InfoLevel))
	assert.Equal(t, "WARN", oteljson.SeverityText(zapcore.WarnLevel))
	assert.Equal(t, 13, oteljson.SeverityNumber(zapcore.WarnLevel))
	assert.Equal(t, "ERROR", oteljson.SeverityText(zapcore.ErrorLevel))
	assert.Equal(t, 17, oteljson.SeverityNumber(zapcore.ErrorLevel))
	assert.Equal(t, "FATAL", oteljson.SeverityText(zapcore.FatalLevel))
	assert.Equal(t, 21, oteljson.SeverityNumber(zapcore.FatalLevel))
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
