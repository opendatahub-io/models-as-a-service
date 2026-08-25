package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestWithBuildNamespace(t *testing.T) {
	t.Run("creates temp wrapper with overlay symlink", func(t *testing.T) {
		path, cleanup, err := withBuildNamespace("/tmp/overlay", "odh-ai-gateway-infra")
		require.NoError(t, err)
		defer cleanup()
		assert.NotEqual(t, "/tmp/overlay", path)
		assert.FileExists(t, path+"/kustomization.yaml")
	})
}

func TestSupportsMetricsParamsPatch(t *testing.T) {
	assert.True(t, supportsMetricsParamsPatch("/repo/maas-api/deploy/overlays/odh"))
	assert.False(t, supportsMetricsParamsPatch("/repo/maas-api/deploy/overlays/xks"))
	assert.False(t, supportsMetricsParamsPatch("/repo/deployment/base/observability"))
}
