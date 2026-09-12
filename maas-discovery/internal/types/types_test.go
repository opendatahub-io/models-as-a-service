package types_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

func TestTenantsResponseJSON(t *testing.T) {
	resp := types.TenantsResponse{
		Tenants: []types.TenantInfo{
			{
				Name: "test-tenant",
				Gateway: types.GatewayMetadata{
					Name:        "maas-default-gateway",
					Namespace:   "openshift-ingress",
					Protocol:    "https",
					ExternalURL: "https://maas.apps.example.com",
					Port:        443,
				},
			},
		},
	}

	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	tenants, ok := raw["tenants"].([]any)
	require.True(t, ok, "expected top-level 'tenants' array")
	require.Len(t, tenants, 1)

	tenant, ok := tenants[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "test-tenant", tenant["name"])

	gw, ok := tenant["gateway"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "maas-default-gateway", gw["name"])
	assert.Equal(t, "openshift-ingress", gw["namespace"])
	assert.Equal(t, "https", gw["protocol"])
	assert.Equal(t, "https://maas.apps.example.com", gw["externalUrl"])
	assert.InDelta(t, 443, gw["port"], 0)
}

func TestTenantsResponseJSONRoundTrip(t *testing.T) {
	original := types.TenantsResponse{
		Tenants: []types.TenantInfo{
			{
				Name: "tenant-a",
				Gateway: types.GatewayMetadata{
					Name:        "gw-a",
					Namespace:   "ns-a",
					Protocol:    "https",
					ExternalURL: "https://a.example.com",
					Port:        8443,
				},
			},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded types.TenantsResponse
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, original, decoded)
}

func TestTenantsResponseEmptyJSON(t *testing.T) {
	resp := types.TenantsResponse{Tenants: []types.TenantInfo{}}

	data, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tenants":[]}`, string(data))
}
