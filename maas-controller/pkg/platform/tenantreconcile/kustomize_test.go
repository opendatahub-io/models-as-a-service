package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
