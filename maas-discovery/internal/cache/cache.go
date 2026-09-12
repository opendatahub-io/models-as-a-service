// Package cache provides read access to tenant metadata.
// The informer-backed implementation will be added in RHOAIENG-90729.
package cache

import (
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

// TenantCache provides read access to tenant metadata.
type TenantCache interface {
	List() []types.TenantInfo
}

// Stub is a placeholder TenantCache that returns an empty tenant list.
type Stub struct{}

// NewStub creates a stub cache.
func NewStub() *Stub {
	return &Stub{}
}

// List returns an empty tenant list.
func (s *Stub) List() []types.TenantInfo {
	return nil
}
