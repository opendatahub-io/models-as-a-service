package cache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
)

func TestCountPartialMetadata(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("healthy", "team-a-gw"),
		makeTenant("degraded", "missing-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("team-a-gw",
			[]any{specListener("HTTPS", "a.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, nil, "gw-ns", testLog)
	require.Len(t, result, 2)

	// One tenant resolves an external URL; the other's gateway is missing,
	// so it degrades to partial metadata.
	assert.Equal(t, 1, cache.CountPartialMetadata(result))
	assert.Empty(t, result[1].Gateway.ExternalURL)
}

func TestCountPartialMetadata_AllHealthy(t *testing.T) {
	tenants := []unstructured.Unstructured{makeTenant("team-a", "team-a-gw")}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("team-a-gw",
			[]any{specListener("HTTPS", "a.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, nil, "gw-ns", testLog)
	assert.Equal(t, 0, cache.CountPartialMetadata(result))
}

func TestCountPartialMetadata_Empty(t *testing.T) {
	assert.Equal(t, 0, cache.CountPartialMetadata(nil))
}
