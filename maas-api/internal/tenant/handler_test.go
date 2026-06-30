//nolint:testpackage // tests access unexported extractGatewayMetadata method
package tenant

import (
	"context"
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
					"hostname":       "maas.apps.example.com",
					"port":           int64(443),
					"protocol":       "HTTPS",
					"attachedRoutes": int64(5),
				},
			},
		},
	}

	ctx := context.Background()
	metadata, err := handler.extractGatewayMetadata(ctx, gateway)

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
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"hostname":       "maas.example.com",
					"port":           int64(8443),
					"protocol":       "HTTPS",
					"attachedRoutes": int64(1),
				},
			},
		},
	}

	ctx := context.Background()
	metadata, err := handler.extractGatewayMetadata(ctx, gateway)

	require.NoError(t, err)
	assert.Equal(t, "https://maas.example.com:8443", metadata.ExternalURL)
	assert.Equal(t, int64(8443), metadata.Port)
}

func TestExtractGatewayMetadata_HTTPListener(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "http",
					"hostname":       "maas.example.com",
					"port":           int64(80),
					"protocol":       "HTTP",
					"attachedRoutes": int64(1),
				},
			},
		},
	}

	ctx := context.Background()
	metadata, err := handler.extractGatewayMetadata(ctx, gateway)

	require.NoError(t, err)
	assert.Equal(t, "http://maas.example.com", metadata.ExternalURL)
	assert.Equal(t, "http", metadata.Protocol)
}

func TestExtractGatewayMetadata_NoListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"status": map[string]any{
			"listeners": []any{},
		},
	}

	ctx := context.Background()
	_, err := handler.extractGatewayMetadata(ctx, gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no listeners")
}

func TestExtractGatewayMetadata_NoReadyListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"status": map[string]any{
			"listeners": []any{
				map[string]any{
					"name":           "https",
					"hostname":       "maas.example.com",
					"port":           int64(443),
					"protocol":       "HTTPS",
					"attachedRoutes": int64(0), // No routes attached
				},
			},
		},
	}

	ctx := context.Background()
	_, err := handler.extractGatewayMetadata(ctx, gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine external hostname")
}
