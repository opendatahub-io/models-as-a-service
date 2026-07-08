//nolint:testpackage // tests access unexported extractGatewayMetadata method
package tenant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// TestGetTenantInfo_Success tests the full HTTP handler.
// Note: The fake dynamic client doesn't properly support Gateway API resources,
// so this test focuses on the handler structure. The core gateway metadata extraction
// logic is thoroughly tested in TestExtractGatewayMetadata_* tests.
func TestGetTenantInfo_Success(t *testing.T) {
	t.Skip("Fake dynamic client doesn't support Gateway GVR - core logic tested via extractGatewayMetadata tests, integration via E2E")
}

func TestExtractGatewayMetadata_Success(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"hostname": "maas.apps.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
			},
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{
					"type":  "Hostname",
					"value": "maas.apps.example.com",
				},
			},
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"attachedRoutes": int64(5),
				},
			},
		},
	}

	metadata, err := handler.extractGatewayMetadata(gateway)

	require.NoError(t, err)
	assert.NotNil(t, metadata)
	assert.Equal(t, "test-gateway", metadata.Name)
	assert.Equal(t, "test-ns", metadata.Namespace)
	assert.Equal(t, "https://maas.apps.example.com", metadata.ExternalURL)
	assert.Equal(t, "https", metadata.Protocol)
	assert.Equal(t, int64(443), metadata.Port)
}

func TestExtractGatewayMetadata_NonStandardPort(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"hostname": "maas.example.com",
					"port":     int64(8443),
					"protocol": "HTTPS",
				},
			},
		},
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"attachedRoutes": int64(1),
				},
			},
		},
	}

	metadata, err := handler.extractGatewayMetadata(gateway)

	require.NoError(t, err)
	assert.Equal(t, "https://maas.example.com:8443", metadata.ExternalURL)
	assert.Equal(t, int64(8443), metadata.Port)
}

func TestExtractGatewayMetadata_HTTPListener(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "http",
					"hostname": "maas.example.com",
					"port":     int64(80),
					"protocol": "HTTP",
				},
			},
		},
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "http",
					"attachedRoutes": int64(1),
				},
			},
		},
	}

	metadata, err := handler.extractGatewayMetadata(gateway)

	require.NoError(t, err)
	assert.Equal(t, "http://maas.example.com", metadata.ExternalURL)
	assert.Equal(t, "http", metadata.Protocol)
}

func TestExtractGatewayMetadata_NoListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{},
		},
		"status": map[string]any{
			"listeners": []any{},
		},
	}

	_, err := handler.extractGatewayMetadata(gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no listeners")
}

func TestExtractGatewayMetadata_NoReadyListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"hostname": "maas.example.com",
					"port":     int64(443),
					"protocol": "HTTPS",
				},
			},
		},
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"attachedRoutes": int64(0), // No routes attached
				},
			},
		},
	}

	_, err := handler.extractGatewayMetadata(gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine external hostname")
}

func TestExtractGatewayMetadata_FallbackToStatusAddresses(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	// Gateway with no listener hostname in spec, should fall back to status.addresses
	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"port":     int64(443),
					"protocol": "HTTPS",
					// No hostname in spec.listeners
				},
			},
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{
					"type":  "IPAddress",
					"value": "gateway.fallback.example.com",
				},
			},
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"attachedRoutes": int64(2),
				},
			},
		},
	}

	metadata, err := handler.extractGatewayMetadata(gateway)

	require.NoError(t, err)
	assert.Equal(t, "https://gateway.fallback.example.com", metadata.ExternalURL)
	assert.Equal(t, "https", metadata.Protocol)
	assert.Equal(t, int64(443), metadata.Port)
}

func TestExtractGatewayMetadata_RejectsInternalServiceName(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	// Gateway with internal service name - should return error, not internal hostname
	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":     "https",
					"port":     int64(443),
					"protocol": "HTTPS",
					// No hostname in spec.listeners
				},
			},
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{
					"type":  "Hostname",
					"value": "test-gateway-openshift-default.openshift-ingress.svc.cluster.local",
				},
			},
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"attachedRoutes": int64(2),
				},
			},
		},
	}

	_, err := handler.extractGatewayMetadata(gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured with an external hostname")
	assert.Contains(t, err.Error(), "internal service name")
}
