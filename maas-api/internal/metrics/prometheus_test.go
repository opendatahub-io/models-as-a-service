package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time verification that PrometheusRecorder implements MetricsRecorder
var _ MetricsRecorder = (*PrometheusRecorder)(nil)

func newTestRecorder(t *testing.T) (*PrometheusRecorder, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	r, err := NewPrometheusRecorder(reg)
	require.NoError(t, err)
	return r, reg
}

func TestRecordRequestDuration(t *testing.T) {
	r, _ := newTestRecorder(t)

	r.RecordRequestDuration("GET", "/v1/models", "200", 150*time.Millisecond)
	r.RecordRequestDuration("GET", "/v1/models", "200", 250*time.Millisecond)
	r.RecordRequestDuration("POST", "/v1/api-keys", "201", 50*time.Millisecond)

	assert.Equal(t, float64(2), testutil.ToFloat64(r.requestsTotal.WithLabelValues("GET", "/v1/models", "200")))
	assert.Equal(t, float64(1), testutil.ToFloat64(r.requestsTotal.WithLabelValues("POST", "/v1/api-keys", "201")))
}

func TestInFlightGauge(t *testing.T) {
	r, _ := newTestRecorder(t)

	r.IncrementInFlight("GET")
	r.IncrementInFlight("GET")
	r.IncrementInFlight("POST")

	assert.Equal(t, float64(2), testutil.ToFloat64(r.inFlight.WithLabelValues("GET")))
	assert.Equal(t, float64(1), testutil.ToFloat64(r.inFlight.WithLabelValues("POST")))

	r.DecrementInFlight("GET")
	assert.Equal(t, float64(1), testutil.ToFloat64(r.inFlight.WithLabelValues("GET")))
}

func TestNewPrometheusRecorderNilRegistry(t *testing.T) {
	r, err := NewPrometheusRecorder(nil)
	assert.Nil(t, r)
	assert.Error(t, err)
}

func TestNewPrometheusRecorderDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewPrometheusRecorder(reg)
	require.NoError(t, err)

	_, err = NewPrometheusRecorder(reg)
	assert.Error(t, err)
}

func TestDurationHistogramObserved(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordRequestDuration("GET", "/v1/models", "200", 150*time.Millisecond)

	families, err := reg.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() == "maas_api_http_request_duration_seconds" {
			found = true
			require.Len(t, f.GetMetric(), 1)
			assert.Equal(t, uint64(1), f.GetMetric()[0].GetHistogram().GetSampleCount())
		}
	}
	assert.True(t, found, "histogram metric not found in registry")
}
