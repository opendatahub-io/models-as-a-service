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

	tests := []struct {
		level  zapcore.Level
		text   string
		number int
	}{
		{zapcore.DebugLevel, "DEBUG", 5},
		{zapcore.InfoLevel, "INFO", 9},
		{zapcore.WarnLevel, "WARN", 13},
		{zapcore.ErrorLevel, "ERROR", 17},
		{zapcore.DPanicLevel, "ERROR2", 18},
		{zapcore.PanicLevel, "ERROR3", 19},
		{zapcore.FatalLevel, "FATAL", 21},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
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
