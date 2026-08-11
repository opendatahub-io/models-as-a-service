package tenantreconcile

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/kustomize/api/resource"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
)

func TestManifestPathForPlatform(t *testing.T) {
	t.Run("returns OCP overlay when isOCP is true", func(t *testing.T) {
		t.Setenv("MAAS_PLATFORM_MANIFESTS", "")
		path := ManifestPathForPlatform(true)
		assert.Equal(t, "/maas-api/deploy/overlays/odh", path)
	})

	t.Run("returns xKS overlay when isOCP is false", func(t *testing.T) {
		t.Setenv("MAAS_PLATFORM_MANIFESTS", "")
		path := ManifestPathForPlatform(false)
		assert.Equal(t, "/maas-api/deploy/overlays/xks", path)
	})

	t.Run("respects MAAS_PLATFORM_MANIFESTS override", func(t *testing.T) {
		t.Setenv("MAAS_PLATFORM_MANIFESTS", "/custom/path")
		path := ManifestPathForPlatform(true)
		assert.Equal(t, "/custom/path", path)
	})
}

func TestRemapServiceMonitorServerName(t *testing.T) {
	const appNamespace = "odh-ai-gateway-infra"

	tests := []struct {
		name       string
		serverName string
		want       string
	}{
		{
			name:       "rewrites when second DNS label is overlay default namespace",
			serverName: "maas-api-metrics.opendatahub.svc",
			want:       "maas-api-metrics." + appNamespace + ".svc",
		},
		{
			name:       "rewrites cluster.local form",
			serverName: "maas-api-metrics.opendatahub.svc.cluster.local",
			want:       "maas-api-metrics." + appNamespace + ".svc.cluster.local",
		},
		{
			name:       "leaves hosts where opendatahub is not the namespace label",
			serverName: "prometheus.monitoring.opendatahub.svc",
			want:       "prometheus.monitoring.opendatahub.svc",
		},
		{
			name:       "leaves already-remapped namespace",
			serverName: "maas-api-metrics." + appNamespace + ".svc",
			want:       "maas-api-metrics." + appNamespace + ".svc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serviceMonitorWithServerName(t, tt.serverName)
			require.NoError(t, remapServiceMonitorServerName(res, appNamespace))

			m, err := res.Map()
			require.NoError(t, err)
			spec, ok := m["spec"].(map[string]any)
			require.True(t, ok)
			endpoints, ok := spec["endpoints"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, endpoints)
			ep, ok := endpoints[0].(map[string]any)
			require.True(t, ok)
			tlsCfg, ok := ep["tlsConfig"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tt.want, tlsCfg["serverName"])
		})
	}
}

func serviceMonitorWithServerName(t *testing.T, serverName string) *resource.Resource {
	t.Helper()
	raw := fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: test-sm
spec:
  endpoints:
  - tlsConfig:
      serverName: %s
`, serverName)
	node, err := kyaml.Parse(raw)
	require.NoError(t, err)
	return &resource.Resource{RNode: *node}
}
