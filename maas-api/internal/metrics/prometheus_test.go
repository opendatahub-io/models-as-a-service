package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
)

func newTestRecorder(t *testing.T) (*metrics.PrometheusRecorder, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	r, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)
	return r, reg
}

// gatherMetricValue finds a metric family by name and returns the value for the given label set.
func gatherMetricValue(t *testing.T, reg *prometheus.Registry, name string, labelValues map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)

	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := make(map[string]string)
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range labelValues {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				if m.GetGauge() != nil {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labelValues)
	return 0
}

func TestRecordRequestDuration(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-a", 150*time.Millisecond)
	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-a", 250*time.Millisecond)
	r.RecordRequestDuration("POST", "/v1/api-keys", "201", "tenant-b", 50*time.Millisecond)

	assert.InDelta(t, float64(2), gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"method": "GET", "route": "/v1/models", "status": "200", "tenant_name": "tenant-a"}), 0)
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"method": "POST", "route": "/v1/api-keys", "status": "201", "tenant_name": "tenant-b"}), 0)
}

// TestRecordRequestDuration_TenantLabel verifies that the same route with different
// tenants produces distinct metric series.
func TestRecordRequestDuration_TenantLabel(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-a", 100*time.Millisecond)
	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-b", 100*time.Millisecond)
	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-b", 100*time.Millisecond)

	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"tenant_name": "tenant-a"}), 0)
	assert.InDelta(t, float64(2), gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"tenant_name": "tenant-b"}), 0)
}

// TestRecordRequestDuration_EmptyTenant verifies that requests without tenant context
// (internal routes, health checks) produce metrics with an empty tenant label.
func TestRecordRequestDuration_EmptyTenant(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordRequestDuration("POST", "/internal/v1/api-keys/validate", "200", "", 10*time.Millisecond)

	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_http_requests_total",
		map[string]string{"tenant_name": "", "route": "/internal/v1/api-keys/validate"}), 0)
}

func TestInFlightGauge(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.IncrementInFlight("GET")
	r.IncrementInFlight("GET")
	r.IncrementInFlight("POST")

	assert.InDelta(t, float64(2), gatherMetricValue(t, reg, "maas_api_http_requests_in_flight", map[string]string{"method": "GET"}), 0)
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_http_requests_in_flight", map[string]string{"method": "POST"}), 0)

	r.DecrementInFlight("GET")
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_http_requests_in_flight", map[string]string{"method": "GET"}), 0)
}

func TestNewPrometheusRecorderNilRegistry(t *testing.T) {
	r, err := metrics.NewPrometheusRecorder(nil)
	assert.Nil(t, r)
	assert.Error(t, err)
}

func TestNewPrometheusRecorderDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)

	_, err = metrics.NewPrometheusRecorder(reg)
	assert.Error(t, err)
}

// TestRecordKeyValidation verifies that the key validation counter increments
// correctly with tenant and result labels.
func TestRecordKeyValidation(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordKeyValidation("redteam", "valid")
	r.RecordKeyValidation("redteam", "valid")
	r.RecordKeyValidation("redteam", "invalid")
	r.RecordKeyValidation("blueteam", "valid")

	assert.InDelta(t, float64(2), gatherMetricValue(t, reg, "maas_api_key_validation_total",
		map[string]string{"tenant_name": "redteam", "result": "valid"}), 0)
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_key_validation_total",
		map[string]string{"tenant_name": "redteam", "result": "invalid"}), 0)
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_key_validation_total",
		map[string]string{"tenant_name": "blueteam", "result": "valid"}), 0)
}

// TestRecordTokenMint verifies that the token mint counter increments
// correctly with tenant and result labels.
func TestRecordTokenMint(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordTokenMint("redteam", "success")
	r.RecordTokenMint("redteam", "success")
	r.RecordTokenMint("redteam", "failure")

	assert.InDelta(t, float64(2), gatherMetricValue(t, reg, "maas_api_token_mint_total",
		map[string]string{"tenant_name": "redteam", "result": "success"}), 0)
	assert.InDelta(t, float64(1), gatherMetricValue(t, reg, "maas_api_token_mint_total",
		map[string]string{"tenant_name": "redteam", "result": "failure"}), 0)
}

func TestDurationHistogramObserved(t *testing.T) {
	r, reg := newTestRecorder(t)

	r.RecordRequestDuration("GET", "/v1/models", "200", "tenant-a", 150*time.Millisecond)

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
