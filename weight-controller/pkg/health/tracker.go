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

// Package health provides cluster health status reading.
// TODO: The actual health monitoring is handled by a separate task.
// This package reads health status from wherever that task publishes it.
// Update this implementation once the health monitoring task design is finalized.
package health

import (
	"context"
)

// State represents the health state of a cluster.
type State string

const (
	StateHealthy   State = "Healthy"
	StateUnhealthy State = "Unhealthy"
	StateUnknown   State = "Unknown"
)

// Source provides cluster health status.
// The actual source of health data depends on the health monitoring task's design.
type Source interface {
	// GetState returns the health state for a provider/cluster.
	GetState(ctx context.Context, providerName string) (State, error)

	// IsHealthy returns true if the cluster is healthy (convenience method).
	IsHealthy(ctx context.Context, providerName string) (bool, error)
}

// TODO: Placeholder implementation - replace once health monitoring task design is finalized.
// Possible implementations:
// - Read from ExternalModel.Status or ExternalProvider.Status field
// - Read from a separate ClusterHealth CRD
// - Read from a ConfigMap
// - Read from annotations

// placeholderSource assumes all clusters are healthy until real implementation is available.
type placeholderSource struct{}

// NewPlaceholderSource creates a placeholder health source that assumes all clusters are healthy.
// Replace with real implementation once health monitoring task design is finalized.
func NewPlaceholderSource() Source {
	return &placeholderSource{}
}

func (p *placeholderSource) GetState(ctx context.Context, providerName string) (State, error) {
	// TODO: Read actual health status from external source
	return StateHealthy, nil
}

func (p *placeholderSource) IsHealthy(ctx context.Context, providerName string) (bool, error) {
	state, err := p.GetState(ctx, providerName)
	if err != nil {
		return false, err
	}
	return state == StateHealthy, nil
}

// Compile-time check that placeholderSource implements Source.
var _ Source = (*placeholderSource)(nil)
