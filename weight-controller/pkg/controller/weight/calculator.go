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

package weight

import (
	"github.com/opendatahub-io/models-as-a-service/weight-controller/pkg/metrics"
)

// Default weights for the calculator algorithm.
const (
	DefaultHealthWeight     = 0.4
	DefaultLatencyWeight    = 0.3
	DefaultQueueDepthWeight = 0.3
)

// Calculator computes optimal weights for providers based on cluster metrics.
type Calculator struct {
	// HealthWeight is the weight given to cluster health in calculations (0-1).
	HealthWeight float64

	// LatencyWeight is the weight given to latency metrics (0-1).
	LatencyWeight float64

	// QueueDepthWeight is the weight given to queue depth metrics (0-1).
	QueueDepthWeight float64
}

// CalculatorOption configures a Calculator.
type CalculatorOption func(*Calculator)

// WithHealthWeight sets the health weight factor.
func WithHealthWeight(w float64) CalculatorOption {
	return func(c *Calculator) {
		if w >= 0 && w <= 1 {
			c.HealthWeight = w
		}
	}
}

// WithLatencyWeight sets the latency weight factor.
func WithLatencyWeight(w float64) CalculatorOption {
	return func(c *Calculator) {
		if w >= 0 && w <= 1 {
			c.LatencyWeight = w
		}
	}
}

// WithQueueDepthWeight sets the queue depth weight factor.
func WithQueueDepthWeight(w float64) CalculatorOption {
	return func(c *Calculator) {
		if w >= 0 && w <= 1 {
			c.QueueDepthWeight = w
		}
	}
}

// NewCalculator creates a Calculator with the given options.
func NewCalculator(opts ...CalculatorOption) *Calculator {
	c := &Calculator{
		HealthWeight:     DefaultHealthWeight,
		LatencyWeight:    DefaultLatencyWeight,
		QueueDepthWeight: DefaultQueueDepthWeight,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ProviderWeight represents the calculated weight for a provider.
type ProviderWeight struct {
	ProviderName string
	Weight       int32
}

// CalculateWeights computes weights for each provider based on their metrics.
// Returns a slice of ProviderWeight with weights normalized to sum to ~100.
//
// TODO(Phase 3): Implement actual weight calculation algorithm based on:
// - Health status (binary: healthy providers get traffic, unhealthy get 0)
// - Queue depth (lower is better)
// - Latency (lower is better)
// - VRAM utilization (lower is better)
func (c *Calculator) CalculateWeights(clusterMetrics map[string]metrics.ClusterMetrics) []ProviderWeight {
	if len(clusterMetrics) == 0 {
		return nil
	}

	results := make([]ProviderWeight, 0, len(clusterMetrics))
	for name := range clusterMetrics {
		results = append(results, ProviderWeight{
			ProviderName: name,
			Weight:       1,
		})
	}

	return results
}
