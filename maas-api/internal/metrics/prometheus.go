package metrics

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type PrometheusRecorder struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inFlight        *prometheus.GaugeVec
	keyValidation   *prometheus.CounterVec
	tokenMint       *prometheus.CounterVec
}

func NewPrometheusRecorder(reg prometheus.Registerer) (*PrometheusRecorder, error) {
	if reg == nil {
		return nil, errors.New("nil prometheus.Registerer")
	}
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maas_api_http_requests_total",
		Help: "Total number of HTTP requests served.",
	}, []string{"method", "route", "status", "tenant_name"})

	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "maas_api_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status", "tenant_name"})

	inFlight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maas_api_http_requests_in_flight",
		Help: "Number of HTTP requests currently being served.",
	}, []string{"method"})

	keyValidation := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maas_api_key_validation_total",
		Help: "Total number of API key validations by tenant and result.",
	}, []string{"tenant_name", "result"})

	tokenMint := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maas_api_token_mint_total",
		Help: "Total number of API key mints by tenant and result.",
	}, []string{"tenant_name", "result"})

	for _, c := range []prometheus.Collector{requestsTotal, requestDuration, inFlight, keyValidation, tokenMint} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	return &PrometheusRecorder{
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
		inFlight:        inFlight,
		keyValidation:   keyValidation,
		tokenMint:       tokenMint,
	}, nil
}

func (r *PrometheusRecorder) RecordRequestDuration(method, route, statusCode, tenant string, duration time.Duration) {
	r.requestsTotal.WithLabelValues(method, route, statusCode, tenant).Inc()
	r.requestDuration.WithLabelValues(method, route, statusCode, tenant).Observe(duration.Seconds())
}

func (r *PrometheusRecorder) RecordKeyValidation(tenant, result string) {
	r.keyValidation.WithLabelValues(tenant, result).Inc()
}

func (r *PrometheusRecorder) RecordTokenMint(tenant, result string) {
	r.tokenMint.WithLabelValues(tenant, result).Inc()
}

func (r *PrometheusRecorder) IncrementInFlight(method string) {
	r.inFlight.WithLabelValues(method).Inc()
}

func (r *PrometheusRecorder) DecrementInFlight(method string) {
	r.inFlight.WithLabelValues(method).Dec()
}
