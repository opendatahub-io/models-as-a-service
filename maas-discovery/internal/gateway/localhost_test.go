package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/gateway"
)

// Exercises the local-hostname detection used to decide whether the OpenShift
// Route fallback should trigger. Mirrors the table from maas-api PR #1888
// (maas-api/internal/tenant/handler_test.go TestIsLocalHostname), expressed
// through ExtractMetadata: local addresses fall through to the Route fallback
// (or the "internal service name" error when no Route matches), while
// non-local addresses are used directly.
func TestExtractMetadata_LocalHostnameDetection(t *testing.T) {
	tests := []struct {
		name    string
		address string
		local   bool
		wantURL string
		wantErr string
		routes  map[string][]gateway.RouteHostInfo
	}{
		{
			name:    "cluster.local suffix triggers fallback",
			address: "internal-gw-svc.gw-ns.svc.cluster.local",
			local:   true,
			routes:  map[string][]gateway.RouteHostInfo{"internal-gw-svc": {{Host: "maas.apps.example.com", Scheme: "https", Port: 443}}},
			wantURL: "https://maas.apps.example.com",
		},
		{
			name:    "svc suffix triggers fallback",
			address: "internal-gw-svc.gw-ns.svc",
			local:   true,
			routes:  map[string][]gateway.RouteHostInfo{"internal-gw-svc": {{Host: "maas.apps.example.com", Scheme: "https", Port: 443}}},
			wantURL: "https://maas.apps.example.com",
		},
		{
			name:    "local suffix triggers fallback",
			address: "internal-gw-svc.gw-ns.local",
			local:   true,
			routes:  map[string][]gateway.RouteHostInfo{"internal-gw-svc": {{Host: "maas.apps.example.com", Scheme: "https", Port: 443}}},
			wantURL: "https://maas.apps.example.com",
		},
		{
			name:    "internal suffix triggers fallback",
			address: "internal-gw-svc.internal",
			local:   true,
			routes:  map[string][]gateway.RouteHostInfo{"internal-gw-svc": {{Host: "maas.apps.example.com", Scheme: "https", Port: 443}}},
			wantURL: "https://maas.apps.example.com",
		},
		{
			name:    "localhost is local",
			address: "localhost",
			local:   true,
			wantErr: "internal service name",
		},
		{
			name:    "loopback IP is local",
			address: "127.0.0.1",
			local:   true,
			wantErr: "internal service name",
		},
		{
			name:    "link-local IP is local",
			address: "169.254.1.1",
			local:   true,
			wantErr: "internal service name",
		},
		{
			name:    "private IP is not local",
			address: "10.0.0.1",
			local:   false,
			wantURL: "https://10.0.0.1",
		},
		{
			name:    "private IP 192.168 is not local",
			address: "192.168.1.1",
			local:   false,
			wantURL: "https://192.168.1.1",
		},
		{
			name:    "external hostname is not local",
			address: "maas.apps.example.com",
			local:   false,
			wantURL: "https://maas.apps.example.com",
		},
		{
			name:    "hostname containing clusterlocal is not local",
			address: "clusterlocal.example.com",
			local:   false,
			wantURL: "https://clusterlocal.example.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gw := makeGateway(
				[]any{httpsListener("", int64(443))},
				[]any{readyStatus("https", int64(1))},
				[]any{map[string]any{"value": test.address}},
			)

			// The Service selecting the gateway is named after the local
			// hostname's first label in these fixtures; it exposes the
			// Gateway's 443 listener port.
			gwServices := make(gateway.GatewayServices, len(test.routes))
			for svcName := range test.routes {
				gwServices[svcName] = []gateway.ServicePort{{Port: 443}}
			}

			meta, err := gateway.ExtractMetadata(gw, "gw", "gw-ns", test.routes, gwServices)
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantURL, meta.ExternalURL)
		})
	}
}
