package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// AuthCacheStats is a snapshot of the auth review cache's cumulative
// counters, as reported by the RHAI-58756 cache's Stats() method.
type AuthCacheStats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

// RegisterAuthCacheStats connects the auth-cache effectiveness metrics to a
// source. Until a source is registered the metrics are absent from the
// endpoint rather than reporting misleading zeros: the review cache
// (RHAI-58756 / PR #1921) is not connected yet on this branch, so main.go
// must call this once the cache exists in this tree.
//
// The collector reads the source on every scrape and exports the counters
// as counter-valued const metrics, which is the idiomatic way to expose
// externally-maintained monotonically increasing counts.
func (m *Metrics) RegisterAuthCacheStats(source func() AuthCacheStats) {
	if m == nil || source == nil {
		return
	}
	m.authCacheMu.Lock()
	defer m.authCacheMu.Unlock()
	m.authCacheSource = source
}

var authCacheDescs = map[string]*prometheus.Desc{
	"discovery_auth_cache_hits_total": prometheus.NewDesc(
		"discovery_auth_cache_hits_total",
		"Total auth review cache hits (token reviews served from cache).",
		nil, nil,
	),
	"discovery_auth_cache_misses_total": prometheus.NewDesc(
		"discovery_auth_cache_misses_total",
		"Total auth review cache misses (token reviews that reached the TokenReview/SAR path).",
		nil, nil,
	),
	"discovery_auth_cache_evictions_total": prometheus.NewDesc(
		"discovery_auth_cache_evictions_total",
		"Total auth review cache evictions (TTL expiry or LRU bound).",
		nil, nil,
	),
}

// authCacheCollector exposes the review cache counters. When no source is
// registered it emits nothing, so the metric families do not appear in
// scrapes at all.
type authCacheCollector struct {
	m *Metrics
}

func (c *authCacheCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range authCacheDescs {
		ch <- d
	}
}

func (c *authCacheCollector) Collect(ch chan<- prometheus.Metric) {
	c.m.authCacheMu.Lock()
	source := c.m.authCacheSource
	c.m.authCacheMu.Unlock()
	if source == nil {
		return
	}

	stats := source()
	ch <- prometheus.MustNewConstMetric(authCacheDescs["discovery_auth_cache_hits_total"], prometheus.CounterValue, float64(stats.Hits))
	ch <- prometheus.MustNewConstMetric(authCacheDescs["discovery_auth_cache_misses_total"], prometheus.CounterValue, float64(stats.Misses))
	ch <- prometheus.MustNewConstMetric(authCacheDescs["discovery_auth_cache_evictions_total"], prometheus.CounterValue, float64(stats.Evictions))
}
