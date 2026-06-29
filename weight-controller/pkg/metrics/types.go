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

package metrics

import (
	"context"
	"time"
)

// ClusterMetrics contains metrics collected from a remote cluster.
type ClusterMetrics struct {
	// ProviderName identifies the ExternalProvider this metrics belongs to.
	ProviderName string

	// Healthy indicates whether the cluster is responsive.
	Healthy bool

	// LastScrapeTime is when these metrics were last collected.
	LastScrapeTime time.Time

	// QueueDepth is the number of pending requests in the model server queue.
	QueueDepth int64

	// P50LatencyMs is the 50th percentile request latency in milliseconds.
	P50LatencyMs float64

	// P99LatencyMs is the 99th percentile request latency in milliseconds.
	P99LatencyMs float64

	// VRAMUtilization is the GPU VRAM utilization as a percentage (0-100).
	VRAMUtilization float64

	// ActiveRequests is the number of currently in-flight requests.
	ActiveRequests int64
}

// Source defines the interface for collecting metrics from a cluster.
type Source interface {
	// Scrape collects metrics from the remote cluster.
	// The context controls cancellation and timeout.
	// Returns an error if the cluster is unreachable or metrics cannot be parsed.
	Scrape(ctx context.Context) (*ClusterMetrics, error)

	// ProviderName returns the name of the ExternalProvider this source collects from.
	ProviderName() string
}
