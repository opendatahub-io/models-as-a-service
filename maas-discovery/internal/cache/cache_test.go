package cache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
)

func TestStubList(t *testing.T) {
	stub := cache.NewStub()
	assert.Empty(t, stub.List())
}

func TestStubImplementsTenantCache(t *testing.T) {
	var _ cache.TenantCache = (*cache.Stub)(nil)
}
