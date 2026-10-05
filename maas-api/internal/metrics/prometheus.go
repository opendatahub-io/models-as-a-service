package metrics

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

type PrometheusRecorder struct {
	requestsTotal         *prometheus.CounterVec
	requestDuration       *prometheus.HistogramVec
	inFlight              *prometheus.GaugeVec
	keyValidation         *prometheus.CounterVec
	tokenMint             *prometheus.CounterVec
	maasRequestsTotal     prometheus.Counter
	maasRequestDuration   prometheus.Histogram
	maasRequestRejections *prometheus.CounterVec
}

var allowedRejectionReasons = map[string]struct{}{
	constant.RejectionRateLimited:   {},
	constant.RejectionUnauthorized:  {},
	constant.RejectionNoCapacity:    {},
	constant.RejectionQuotaExceeded: {},
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

	// Unlabeled so a representative scrape stays under the 20-series ceiling.
	// Histogram (not HistogramVec) materializes every bucket at Register.
	maasRequestsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "maas_requests_total",
		Help: "Total number of HTTP requests served.",
	})

	maasRequestDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "maas_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	maasRequestRejections := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maas_request_rejections_total",
		Help: "Total number of routing rejections by reason.",
	}, []string{"reason"})

	for _, c := range []prometheus.Collector{requestsTotal, requestDuration, inFlight, keyValidation, tokenMint, maasRequestsTotal, maasRequestDuration, maasRequestRejections} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	maasRequestRejections.WithLabelValues(constant.RejectionRateLimited)
	maasRequestRejections.WithLabelValues(constant.RejectionUnauthorized)
	maasRequestRejections.WithLabelValues(constant.RejectionNoCapacity)
	maasRequestRejections.WithLabelValues(constant.RejectionQuotaExceeded)

	return &PrometheusRecorder{
		requestsTotal:         requestsTotal,
		requestDuration:       requestDuration,
		inFlight:              inFlight,
		keyValidation:         keyValidation,
		tokenMint:             tokenMint,
		maasRequestsTotal:     maasRequestsTotal,
		maasRequestDuration:   maasRequestDuration,
		maasRequestRejections: maasRequestRejections,
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

func (r *PrometheusRecorder) RecordRequest(duration time.Duration) {
	r.maasRequestsTotal.Inc()
	r.maasRequestDuration.Observe(duration.Seconds())
}

func (r *PrometheusRecorder) RecordRejection(reason string) {
	if _, ok := allowedRejectionReasons[reason]; !ok {
		return
	}
	r.maasRequestRejections.WithLabelValues(reason).Inc()
}
