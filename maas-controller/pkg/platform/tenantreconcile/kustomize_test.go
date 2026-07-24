package tenantreconcile

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultManifestPath(t *testing.T) {
	tests := []struct {
		name         string
		envPlatform  string // MAAS_PLATFORM_MANIFESTS
		envType      string // ODH_MODULE_OPERATOR_PLATFORM_TYPE
		expectedPath string
	}{
		{
			name:         "explicit MAAS_PLATFORM_MANIFESTS takes precedence",
			envPlatform:  "/custom/overlay",
			envType:      "",
			expectedPath: "/custom/overlay",
		},
		{
			name:         "MAAS_PLATFORM_MANIFESTS takes precedence over XKS platform type",
			envPlatform:  "/custom/overlay",
			envType:      "XKS",
			expectedPath: "/custom/overlay",
		},
		{
			name:         "XKS platform type selects xks overlay",
			envPlatform:  "",
			envType:      "XKS",
			expectedPath: "../maas-api/deploy/overlays/xks",
		},
		{
			name:         "xks platform type is case-insensitive",
			envPlatform:  "",
			envType:      "xks",
			expectedPath: "../maas-api/deploy/overlays/xks",
		},
		{
			name:         "Xks platform type is case-insensitive",
			envPlatform:  "",
			envType:      "Xks",
			expectedPath: "../maas-api/deploy/overlays/xks",
		},
		{
			name:         "default falls back to odh overlay",
			envPlatform:  "",
			envType:      "",
			expectedPath: "../maas-api/deploy/overlays/odh",
		},
		{
			name:         "non-XKS platform type falls back to odh overlay",
			envPlatform:  "",
			envType:      "OpenShift",
			expectedPath: "../maas-api/deploy/overlays/odh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envPlatform != "" {
				t.Setenv("MAAS_PLATFORM_MANIFESTS", tt.envPlatform)
			} else {
				// t.Setenv registers cleanup that correctly restores (or unsets) the original value.
				// Follow with os.Unsetenv so the var is absent during the test.
				t.Setenv("MAAS_PLATFORM_MANIFESTS", "")
				os.Unsetenv("MAAS_PLATFORM_MANIFESTS")
			}

			if tt.envType != "" {
				t.Setenv("ODH_MODULE_OPERATOR_PLATFORM_TYPE", tt.envType)
			} else {
				t.Setenv("ODH_MODULE_OPERATOR_PLATFORM_TYPE", "")
				os.Unsetenv("ODH_MODULE_OPERATOR_PLATFORM_TYPE")
			}

			got := DefaultManifestPath()
			assert.Equal(t, tt.expectedPath, got)
		})
	}
}
