package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	t.Run("rewrites serverName for maas-api-metrics", func(t *testing.T) {
		node, err := kyaml.Parse(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: maas-api-metrics
spec:
  endpoints:
    - tlsConfig:
        serverName: maas-api-metrics.opendatahub.svc
`)
		require.NoError(t, err)

		res := &resource.Resource{RNode: *node}
		require.NoError(t, remapServiceMonitorServerName(res, "odh-ai-gateway-infra"))

		m, err := res.Map()
		require.NoError(t, err)
		endpoints, found, err := unstructured.NestedSlice(m, "spec", "endpoints")
		require.NoError(t, err)
		require.True(t, found)
		require.NotEmpty(t, endpoints)
		ep, ok := endpoints[0].(map[string]any)
		require.True(t, ok)
		got, found, err := unstructured.NestedString(ep, "tlsConfig", "serverName")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "maas-api-metrics.odh-ai-gateway-infra.svc", got)
	})

	t.Run("ignores other ServiceMonitors", func(t *testing.T) {
		node, err := kyaml.Parse(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: other-metrics
spec:
  endpoints:
    - tlsConfig:
        serverName: other-metrics.opendatahub.svc
`)
		require.NoError(t, err)

		res := &resource.Resource{RNode: *node}
		require.NoError(t, remapServiceMonitorServerName(res, "odh-ai-gateway-infra"))

		m, err := res.Map()
		require.NoError(t, err)
		endpoints, found, err := unstructured.NestedSlice(m, "spec", "endpoints")
		require.NoError(t, err)
		require.True(t, found)
		require.NotEmpty(t, endpoints)
		ep, ok := endpoints[0].(map[string]any)
		require.True(t, ok)
		got, found, err := unstructured.NestedString(ep, "tlsConfig", "serverName")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "other-metrics.opendatahub.svc", got)
	})
}
