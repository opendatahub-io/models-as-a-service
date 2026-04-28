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
}

func NewPrometheusRecorder(reg prometheus.Registerer) (*PrometheusRecorder, error) {
	if reg == nil {
		return nil, errors.New("nil prometheus.Registerer")
	}
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "maas_api_http_requests_total",
		Help: "Total number of HTTP requests served.",
	}, []string{"method", "route", "status"})

	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "maas_api_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})

	inFlight := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "maas_api_http_requests_in_flight",
		Help: "Number of HTTP requests currently being served.",
	}, []string{"method"})

	for _, c := range []prometheus.Collector{requestsTotal, requestDuration, inFlight} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	return &PrometheusRecorder{
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
		inFlight:        inFlight,
	}, nil
}

func (r *PrometheusRecorder) RecordRequestDuration(method, route, statusCode string, duration time.Duration) {
	r.requestsTotal.WithLabelValues(method, route, statusCode).Inc()
	r.requestDuration.WithLabelValues(method, route, statusCode).Observe(duration.Seconds())
}

func (r *PrometheusRecorder) IncrementInFlight(method string) {
	r.inFlight.WithLabelValues(method).Inc()
}

func (r *PrometheusRecorder) DecrementInFlight(method string) {
	r.inFlight.WithLabelValues(method).Dec()
}
