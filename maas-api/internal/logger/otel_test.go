package logger_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
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
		{zapcore.FatalLevel, "FATAL", 21},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.text, logger.SeverityText(tt.level))
			assert.Equal(t, tt.number, logger.SeverityNumber(tt.level))
		})
	}
}

func TestProductionJSONUsesOTelFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := zapcore.NewJSONEncoder(logger.EncoderConfig())
	core := logger.WrapCore(zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel))
	zl := zap.New(core).Sugar()
	zl.Infow("request completed")
	require.NoError(t, zl.Sync())

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "request completed", rec["body"])
	assert.Equal(t, "INFO", rec["severity_text"])
	assert.InDelta(t, float64(9), rec["severity_number"], 0)
	assert.NotEmpty(t, rec["timestamp"])
}

func TestWithContextInjectsTraceIDs(t *testing.T) {
	t.Parallel()

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	ctx, span := tp.Tracer("test").Start(t.Context(), "handler")
	defer span.End()

	fields := logger.TraceFields(ctx)
	require.Len(t, fields, 4)
	assert.Equal(t, "trace_id", fields[0])
	assert.Equal(t, span.SpanContext().TraceID().String(), fields[1])
	assert.Equal(t, "span_id", fields[2])
	assert.Equal(t, span.SpanContext().SpanID().String(), fields[3])
}

func TestTraceFieldsEmptyWithoutSpan(t *testing.T) {
	t.Parallel()
	assert.Nil(t, logger.TraceFields(t.Context()))
}
