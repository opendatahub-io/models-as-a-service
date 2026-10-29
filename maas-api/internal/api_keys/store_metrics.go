package api_keys

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/env"
)

const (
	defaultReplicationLagWarningSeconds  = 5.0
	defaultReplicationLagCriticalSeconds = 30.0
)

var (
	databaseQueriesDesc = prometheus.NewDesc(
		"maas_api_database_queries_total",
		"Total number of API-key database queries by operation and selected pool.",
		[]string{"operation", "pool"}, nil,
	)
	databaseFallbacksDesc = prometheus.NewDesc(
		"maas_api_database_read_fallbacks_total",
		"Total number of read queries retried on the writable primary after a read-pool failure.",
		nil, nil,
	)
	databasePingFailuresDesc = prometheus.NewDesc(
		"maas_api_database_ping_failures_total",
		"Total number of failed database health pings by pool.",
		[]string{"pool"}, nil,
	)
	databaseConnectionsDesc = prometheus.NewDesc(
		"maas_api_database_connections",
		"Current database connection counts by pool and state.",
		[]string{"pool", "state"}, nil,
	)
	replicationLagDesc = prometheus.NewDesc(
		"maas_api_database_replication_lag_seconds",
		"Age of the last replayed PostgreSQL transaction on the read replica.",
		nil, nil,
	)
	replicationLagWarningDesc = prometheus.NewDesc(
		"maas_api_database_replication_lag_warning_seconds",
		"Configured warning threshold for PostgreSQL read-replica lag.",
		nil, nil,
	)
	replicationLagCriticalDesc = prometheus.NewDesc(
		"maas_api_database_replication_lag_critical_seconds",
		"Configured critical threshold for PostgreSQL read-replica lag.",
		nil, nil,
	)
)

type queryMetricKey struct {
	operation string
	pool      string
}

// storeMetrics is a lightweight collector rather than a set of package-global metrics.
// Each store owns its counters so tests and future multi-store processes cannot
// accidentally share or double-register database state.
type storeMetrics struct {
	// sync.Map avoids taking a global mutex on every GetByHash validation. Its
	// LoadOrStore fast path is optimized for the steady state where the fixed
	// operation/pool keys already exist; counter updates are atomic. Collection can
	// snapshot concurrently without blocking request routing.
	queries      sync.Map
	pingFailures sync.Map
	fallbacks    atomic.Uint64
	lagWarning   float64
	lagCritical  float64
}

func newStoreMetrics() *storeMetrics {
	return &storeMetrics{
		lagWarning:  configuredLagThreshold("DB_READ_REPLICA_LAG_WARNING_SECONDS", defaultReplicationLagWarningSeconds),
		lagCritical: configuredLagThreshold("DB_READ_REPLICA_LAG_CRITICAL_SECONDS", defaultReplicationLagCriticalSeconds),
	}
}

func configuredLagThreshold(name string, fallback float64) float64 {
	// Invalid and negative values intentionally fall back to the default. Thresholds are
	// alerting metadata, not routing inputs, so malformed monitoring configuration must
	// not prevent maas-api from serving database traffic.
	value, err := strconv.ParseFloat(env.GetString(name, ""), 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func (s *PostgresStore) recordQuery(operation, pool string) {
	if s.metrics == nil {
		return
	}
	key := queryMetricKey{operation: operation, pool: pool}
	counterValue, _ := s.metrics.queries.LoadOrStore(key, &atomic.Uint64{})
	counter, ok := counterValue.(*atomic.Uint64)
	if !ok {
		return
	}
	counter.Add(1)
}

func (s *PostgresStore) recordReadFallback() {
	if s.metrics != nil {
		s.metrics.fallbacks.Add(1)
	}
}

func (s *PostgresStore) recordPingFailure(pool string) {
	if s.metrics == nil {
		return
	}
	counterValue, _ := s.metrics.pingFailures.LoadOrStore(pool, &atomic.Uint64{})
	counter, ok := counterValue.(*atomic.Uint64)
	if !ok {
		return
	}
	counter.Add(1)
}

// Describe implements prometheus.Collector.
func (s *PostgresStore) Describe(ch chan<- *prometheus.Desc) {
	ch <- databaseQueriesDesc
	ch <- databaseFallbacksDesc
	ch <- databasePingFailuresDesc
	ch <- databaseConnectionsDesc
	ch <- replicationLagDesc
	ch <- replicationLagWarningDesc
	ch <- replicationLagCriticalDesc
}

// Collect implements prometheus.Collector. Database pings and the lag query are
// bounded so a slow or unavailable database cannot block metrics collection.
func (s *PostgresStore) Collect(ch chan<- prometheus.Metric) {
	if s.metrics == nil {
		return
	}
	queries := make(map[queryMetricKey]uint64)
	s.metrics.queries.Range(func(key, value any) bool {
		metricKey, ok := key.(queryMetricKey)
		if !ok {
			return true
		}
		counter, ok := value.(*atomic.Uint64)
		if ok {
			queries[metricKey] = counter.Load()
		}
		return true
	})
	pingFailures := make(map[string]uint64)
	s.metrics.pingFailures.Range(func(key, value any) bool {
		pool, ok := key.(string)
		if !ok {
			return true
		}
		counter, ok := value.(*atomic.Uint64)
		if ok {
			pingFailures[pool] = counter.Load()
		}
		return true
	})
	fallbacks := s.metrics.fallbacks.Load()
	lagWarning := s.metrics.lagWarning
	lagCritical := s.metrics.lagCritical

	for key, count := range queries {
		ch <- prometheus.MustNewConstMetric(databaseQueriesDesc, prometheus.CounterValue,
			float64(count), key.operation, key.pool)
	}
	ch <- prometheus.MustNewConstMetric(databaseFallbacksDesc, prometheus.CounterValue,
		float64(fallbacks))
	for pool, count := range pingFailures {
		ch <- prometheus.MustNewConstMetric(databasePingFailuresDesc, prometheus.CounterValue,
			float64(count), pool)
	}

	s.collectConnectionStats(ch, "write", s.writeDB)
	if s.hasReadReplica() {
		s.collectConnectionStats(ch, "read", s.readDB)
	}
	s.collectPings()
	if s.hasReadReplica() {
		s.collectReplicationLag(ch)
	}
	ch <- prometheus.MustNewConstMetric(replicationLagWarningDesc, prometheus.GaugeValue,
		lagWarning)
	ch <- prometheus.MustNewConstMetric(replicationLagCriticalDesc, prometheus.GaugeValue,
		lagCritical)
}

func (s *PostgresStore) collectConnectionStats(ch chan<- prometheus.Metric, pool string, db *sql.DB) {
	if db == nil {
		return
	}
	stats := db.Stats()
	ch <- prometheus.MustNewConstMetric(databaseConnectionsDesc, prometheus.GaugeValue,
		float64(stats.OpenConnections), pool, "open")
	ch <- prometheus.MustNewConstMetric(databaseConnectionsDesc, prometheus.GaugeValue,
		float64(stats.InUse), pool, "in_use")
	ch <- prometheus.MustNewConstMetric(databaseConnectionsDesc, prometheus.GaugeValue,
		float64(stats.Idle), pool, "idle")
}

func (s *PostgresStore) collectPings() {
	// Each pool gets an independent timeout. Sharing one context would let a slow or
	// unavailable primary consume the deadline and fail the replica ping before that
	// pool is actually tested, incorrectly degrading a healthy read pool.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), time.Second)
	defer writeCancel()
	if err := s.writeDB.PingContext(writeCtx); err != nil {
		s.recordPingFailure("write")
	}
	if s.hasReadReplica() {
		readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
		defer readCancel()
		if err := s.readDB.PingContext(readCtx); err != nil {
			s.recordPingFailure("read")
			s.readHealthy.Store(false)
		}
		// A successful ping does not set readHealthy. Query failures remain visible until
		// a subsequent direct read succeeds; TCP reachability does not prove that queries
		// can complete or that the replica is still serving the expected schema.
	}
}

func (s *PostgresStore) collectReplicationLag(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var lag sql.NullFloat64
	query := `SELECT EXTRACT(EPOCH FROM (clock_timestamp() - pg_last_xact_replay_timestamp()))::double precision`
	// A NULL replay timestamp is valid on an idle replica and means there is no observed
	// transaction to age. Permission or query failures omit the gauge rather than failing
	// the entire metrics scrape; the ping-failure and routing counters provide the health
	// signals that do not depend on monitoring-role privileges.
	if err := s.readDB.QueryRowContext(ctx, query).Scan(&lag); err != nil || !lag.Valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(replicationLagDesc, prometheus.GaugeValue, lag.Float64)
}

// DatabaseHealth reports read-pool status without exposing connection URLs or
// credentials. A degraded replica does not make the process health endpoint fail.
func (s *PostgresStore) DatabaseHealth() map[string]any {
	if !s.hasReadReplica() {
		return map[string]any{
			"read_replica_configured": false,
			"read_pool_healthy":       false,
		}
	}
	return map[string]any{
		"read_replica_configured": true,
		"read_pool_healthy":       s.readHealthy.Load(),
	}
}
