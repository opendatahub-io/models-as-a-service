package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/metrics"
)

// findMetric gathers the registry and returns the value of the metric with
// the given name and labels.
func findMetric(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				switch mf.GetType() {
				case dto.MetricType_COUNTER:
					return metric.GetCounter().GetValue()
				case dto.MetricType_GAUGE:
					return metric.GetGauge().GetValue()
				case dto.MetricType_HISTOGRAM:
					return float64(metric.GetHistogram().GetSampleCount())
				default:
					t.Fatalf("unsupported metric type %v for %s", mf.GetType(), name)
				}
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func labelsMatch(lbls []*dto.LabelPair, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range lbls {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func hasMetric(t *testing.T, m *metrics.Metrics, name string) bool {
	t.Helper()
	families, err := m.Registry().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == name {
			return true
		}
	}
	return false
}

func TestObserveRequest_RecordsExactStatusCode(t *testing.T) {
	m := metrics.New()

	m.ObserveRequest(200, "/v1/tenants", 10*time.Millisecond)
	m.ObserveRequest(200, "/v1/tenants", 20*time.Millisecond)
	m.ObserveRequest(403, "/v1/tenants", 5*time.Millisecond)

	assert.InDelta(t, 2.0, findMetric(t, m, "discovery_request_total", map[string]string{"code": "200"}), 1e-9)
	assert.InDelta(t, 1.0, findMetric(t, m, "discovery_request_total", map[string]string{"code": "403"}), 1e-9)

	// The 401/403 distinction is the diagnostic signal, so codes stay exact.
	assert.InDelta(t, 1.0, findMetric(t, m, "discovery_request_duration_seconds", map[string]string{"code": "403"}), 1e-9)
}

func TestObserveRequest_IgnoresUnmatchedPaths(t *testing.T) {
	m := metrics.New()
	m.ObserveRequest(200, "", time.Millisecond)
	m.ObserveRequest(404, "", time.Millisecond)
	assert.False(t, hasMetric(t, m, "discovery_request_total"))
}

func TestSetTenantCounts(t *testing.T) {
	m := metrics.New()
	m.SetTenantCounts(7, 2)
	assert.InDelta(t, 7.0, findMetric(t, m, "discovery_cache_tenants", nil), 1e-9)
	assert.InDelta(t, 2.0, findMetric(t, m, "discovery_tenants_partial_metadata", nil), 1e-9)
}

func TestInformerSynced(t *testing.T) {
	m := metrics.New()
	m.SetInformerSynced(metrics.ResourceAITenants, true)
	m.SetInformerSynced(metrics.ResourceRoutes, false)
	m.IncrInformerSyncTimeout(metrics.ResourceRoutes)
	m.IncrInformerSyncTimeout(metrics.ResourceRoutes)

	assert.InDelta(t, 1.0, findMetric(t, m, "discovery_informer_synced", map[string]string{"resource": "aitenants"}), 1e-9)
	assert.InDelta(t, 0.0, findMetric(t, m, "discovery_informer_synced", map[string]string{"resource": "routes"}), 1e-9)
	assert.InDelta(t, 2.0, findMetric(t, m, "discovery_informer_sync_timeouts_total", map[string]string{"resource": "routes"}), 1e-9)
}

func TestAuthCacheStats_AbsentUntilSourceRegistered(t *testing.T) {
	m := metrics.New()
	assert.False(t, hasMetric(t, m, "discovery_auth_cache_hits_total"),
		"dormant collector must not emit misleading zeros")

	m.RegisterAuthCacheStats(func() metrics.AuthCacheStats {
		return metrics.AuthCacheStats{Hits: 5, Misses: 3, Evictions: 1}
	})

	assert.InDelta(t, 5.0, findMetric(t, m, "discovery_auth_cache_hits_total", nil), 1e-9)
	assert.InDelta(t, 3.0, findMetric(t, m, "discovery_auth_cache_misses_total", nil), 1e-9)
	assert.InDelta(t, 1.0, findMetric(t, m, "discovery_auth_cache_evictions_total", nil), 1e-9)
}

func TestAuthCacheStats_ReadsSourceOnEachScrape(t *testing.T) {
	m := metrics.New()
	stats := metrics.AuthCacheStats{Hits: 1}
	m.RegisterAuthCacheStats(func() metrics.AuthCacheStats { return stats })

	assert.InDelta(t, 1.0, findMetric(t, m, "discovery_auth_cache_hits_total", nil), 1e-9)

	stats.Hits = 42 // counters are read at scrape time, not snapshotted
	assert.InDelta(t, 42.0, findMetric(t, m, "discovery_auth_cache_hits_total", nil), 1e-9)
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *metrics.Metrics
	assert.NotPanics(t, func() {
		m.ObserveRequest(200, "/v1/tenants", time.Millisecond)
		m.SetTenantCounts(1, 0)
		m.SetInformerSynced(metrics.ResourceRoutes, true)
		m.IncrInformerSyncTimeout(metrics.ResourceRoutes)
		m.RegisterAuthCacheStats(nil)
	})
}

func TestHandler_ServesPrometheusTextFormat(t *testing.T) {
	m := metrics.New()
	m.SetInformerSynced(metrics.ResourceAITenants, true)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "discovery_informer_synced")
	assert.Contains(t, string(body), "go_goroutines", "Go runtime collectors should be registered")
}
