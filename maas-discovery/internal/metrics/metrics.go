// Package metrics defines the maas-discovery Prometheus metrics. All metrics
// are served from a dedicated registry on a separate cluster-internal port;
// they are never exposed through the tenant-facing gateway route.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Resource label values for the per-informer-family metrics.
const (
	ResourceAITenants    = "aitenants"
	ResourceGateways     = "gateways"
	ResourceRoutes       = "routes"
	ResourceEntitlements = "entitlements"
)

// requestDurationBuckets are tuned for the RHOAIENG-90735 p99 <50ms target:
// fine-grained below 100ms, where the interesting latency lives (an auth
// cache miss costs a TokenReview plus SARs against the API server), with
// enough range above to see degradation.
var requestDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// Metrics holds the discovery service's Prometheus metrics on a dedicated
// registry. All methods tolerate a nil receiver so callers that do not need
// metrics (tests, embedded use) can pass nil without guards.
type Metrics struct {
	registry *prometheus.Registry

	requestTotal    *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	cacheTenants    prometheus.Gauge
	tenantsPartial  prometheus.Gauge
	informerSynced  *prometheus.GaugeVec
	syncTimeouts    *prometheus.CounterVec

	authCacheMu     sync.Mutex
	authCacheSource func() AuthCacheStats
}

// New creates a Metrics with its own registry, including the standard Go
// runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requestTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_request_total",
			Help: "Total number of discovery API requests by HTTP status code.",
		}, []string{"code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "discovery_request_duration_seconds",
			Help:    "Duration of discovery API requests, including auth middleware, by HTTP status code.",
			Buckets: requestDurationBuckets,
		}, []string{"code"}),
		cacheTenants: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "discovery_cache_tenants",
			Help: "Number of tenants currently held in the informer cache.",
		}),
		tenantsPartial: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "discovery_tenants_partial_metadata",
			Help: "Number of cached tenants with partial gateway metadata (missing external URL).",
		}),
		informerSynced: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "discovery_informer_synced",
			Help: "Whether the informer for a resource family has completed its initial sync (1) or is degraded (0).",
		}, []string{"resource"}),
		syncTimeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "discovery_informer_sync_timeouts_total",
			Help: "Total number of informer initial-sync timeouts per resource family (service continues in degraded mode).",
		}, []string{"resource"}),
	}

	m.registry.MustRegister(
		m.requestTotal,
		m.requestDuration,
		m.cacheTenants,
		m.tenantsPartial,
		m.informerSynced,
		m.syncTimeouts,
		&authCacheCollector{m: m},
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Registry returns the dedicated Prometheus registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// ObserveRequest records one completed request. route is the gin route
// template (c.FullPath()); requests to unmatched paths are ignored. The
// exact status code is kept as the label: the 401/403/500 distinction is
// the diagnostic signal (auth misconfiguration vs RBAC expectations vs
// server trouble).
func (m *Metrics) ObserveRequest(code int, route string, d time.Duration) {
	if m == nil || route == "" {
		return
	}
	m.requestTotal.WithLabelValues(strconv.Itoa(code)).Inc()
	m.requestDuration.WithLabelValues(strconv.Itoa(code)).Observe(d.Seconds())
}

// SetTenantCounts updates the cache gauges: total cached tenants and how
// many of those have partial gateway metadata.
func (m *Metrics) SetTenantCounts(total, partialMetadata int) {
	if m == nil {
		return
	}
	m.cacheTenants.Set(float64(total))
	m.tenantsPartial.Set(float64(partialMetadata))
}

// SetInformerSynced records whether a resource family's informer completed
// its initial sync.
func (m *Metrics) SetInformerSynced(resource string, synced bool) {
	if m == nil {
		return
	}
	v := 0.0
	if synced {
		v = 1
	}
	m.informerSynced.WithLabelValues(resource).Set(v)
}

// IncrInformerSyncTimeout records one initial-sync timeout for a resource
// family where the service continues in degraded mode.
func (m *Metrics) IncrInformerSyncTimeout(resource string) {
	if m == nil {
		return
	}
	m.syncTimeouts.WithLabelValues(resource).Inc()
}

// Handler returns an HTTP handler exposing the registry in the Prometheus
// text format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
