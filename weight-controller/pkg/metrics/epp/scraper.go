/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package epp provides metrics scraping for llm-d EPP (Endpoint Payload Processor).
package epp

import (
	"context"
	"time"

	"github.com/opendatahub-io/models-as-a-service/weight-controller/pkg/metrics"
)

// Compile-time interface compliance check.
var _ metrics.Source = (*Scraper)(nil)

// DefaultTimeout is the default HTTP timeout for scraping metrics.
const DefaultTimeout = 5 * time.Second

// Scraper collects Prometheus metrics from an llm-d EPP instance.
type Scraper struct {
	providerName string
	endpoint     string
	timeout      time.Duration
}

// Option configures a Scraper.
type Option func(*Scraper)

// WithTimeout sets the HTTP timeout for scraping.
func WithTimeout(timeout time.Duration) Option {
	return func(s *Scraper) {
		if timeout > 0 {
			s.timeout = timeout
		}
	}
}

// NewScraper creates a new EPP metrics scraper.
func NewScraper(providerName, endpoint string, opts ...Option) *Scraper {
	s := &Scraper{
		providerName: providerName,
		endpoint:     endpoint,
		timeout:      DefaultTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Scrape collects metrics from the EPP Prometheus endpoint.
//
// TODO(Phase 2): Implement actual Prometheus scraping:
// - HTTP GET to endpoint/metrics (typically :9090/metrics for EPP)
// - Parse Prometheus text format
// - Extract relevant metrics:
//   - epp_request_latency_seconds (histogram)
//   - epp_requests_total (counter)
//   - epp_active_requests (gauge)
func (s *Scraper) Scrape(ctx context.Context) (*metrics.ClusterMetrics, error) {
	_ = ctx // Will be used for HTTP request cancellation in Phase 2
	return &metrics.ClusterMetrics{
		ProviderName:    s.providerName,
		Healthy:         true,
		LastScrapeTime:  time.Now(),
		QueueDepth:      0,
		P50LatencyMs:    0,
		P99LatencyMs:    0,
		VRAMUtilization: 0,
		ActiveRequests:  0,
	}, nil
}

// ProviderName returns the ExternalProvider name.
func (s *Scraper) ProviderName() string {
	return s.providerName
}
