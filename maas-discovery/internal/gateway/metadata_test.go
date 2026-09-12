package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/gateway"
)

func makeGateway(listeners, statusListeners, addresses []any) map[string]any {
	status := map[string]any{
		"listeners": statusListeners,
	}
	if addresses != nil {
		status["addresses"] = addresses
	}
	return map[string]any{
		"spec": map[string]any{
			"listeners": listeners,
		},
		"status": status,
	}
}

func httpsListener(hostname string, port int64) map[string]any {
	l := map[string]any{
		"name":     "https",
		"protocol": "HTTPS",
		"port":     port,
	}
	if hostname != "" {
		l["hostname"] = hostname
	}
	return l
}

func httpListener(hostname string, port int64) map[string]any {
	l := map[string]any{
		"name":     "http",
		"protocol": "HTTP",
		"port":     port,
	}
	if hostname != "" {
		l["hostname"] = hostname
	}
	return l
}

func readyStatus(name string, attachedRoutes int64) map[string]any {
	return map[string]any{
		"name":           name,
		"attachedRoutes": attachedRoutes,
	}
}

func TestExtractMetadata_HTTPS(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(1))},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.NoError(t, err)
	assert.Equal(t, "my-gw", meta.Name)
	assert.Equal(t, "gw-ns", meta.Namespace)
	assert.Equal(t, "https", meta.Protocol)
	assert.Equal(t, "https://gw.example.com", meta.ExternalURL)
	assert.Equal(t, int64(443), meta.Port)
}

func TestExtractMetadata_HTTPSNonStandardPort(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(8443))},
		[]any{readyStatus("https", int64(1))},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example.com:8443", meta.ExternalURL)
	assert.Equal(t, int64(8443), meta.Port)
}

func TestExtractMetadata_HTTP(t *testing.T) {
	gw := makeGateway(
		[]any{httpListener("gw.example.com", int64(80))},
		[]any{readyStatus("http", int64(1))},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.NoError(t, err)
	assert.Equal(t, "http", meta.Protocol)
	assert.Equal(t, "http://gw.example.com", meta.ExternalURL)
	assert.Equal(t, int64(80), meta.Port)
}

func TestExtractMetadata_PrefersHTTPSOverHTTP(t *testing.T) {
	gw := makeGateway(
		[]any{
			httpListener("gw.example.com", int64(80)),
			httpsListener("gw.example.com", int64(443)),
		},
		[]any{
			readyStatus("http", int64(1)),
			readyStatus("https", int64(1)),
		},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.NoError(t, err)
	assert.Equal(t, "https", meta.Protocol)
	assert.Equal(t, int64(443), meta.Port)
}

func TestExtractMetadata_FallsBackToStatusAddresses(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "lb.example.com", "type": "Hostname"}},
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.NoError(t, err)
	assert.Equal(t, "https://lb.example.com", meta.ExternalURL)
}

func TestExtractMetadata_RejectsInternalHostname(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-svc.default.svc.cluster.local"}},
	)

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal service name")
}

func TestExtractMetadata_NoExternalHostname(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		nil,
	)

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine external hostname")
}

func TestExtractMetadata_NoSpec(t *testing.T) {
	gw := map[string]any{"status": map[string]any{}}
	_, err := gateway.ExtractMetadata(gw, "gw", "ns")
	assert.Error(t, err)
}

func TestExtractMetadata_NoStatus(t *testing.T) {
	gw := map[string]any{"spec": map[string]any{"listeners": []any{}}}
	_, err := gateway.ExtractMetadata(gw, "gw", "ns")
	assert.Error(t, err)
}

func TestExtractMetadata_NoListeners(t *testing.T) {
	gw := makeGateway(
		[]any{},
		[]any{readyStatus("https", int64(1))},
		nil,
	)
	_, err := gateway.ExtractMetadata(gw, "gw", "ns")
	assert.Error(t, err)
}

func TestExtractMetadata_NoReadyListeners(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(0))},
		nil,
	)

	_, err := gateway.ExtractMetadata(gw, "gw", "ns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ready listeners")
}

func TestExtractMetadata_NoReadyListenersWithAddresses(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(0))},
		[]any{map[string]any{"value": "lb.example.com", "type": "Hostname"}},
	)

	_, err := gateway.ExtractMetadata(gw, "gw", "ns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ready listeners",
		"should not produce a valid URL when no listener has attached routes")
}

func TestExtractMetadata_Float64Port(t *testing.T) {
	gw := makeGateway(
		[]any{map[string]any{
			"name":     "https",
			"protocol": "HTTPS",
			"port":     float64(8443),
			"hostname": "gw.example.com",
		}},
		[]any{map[string]any{
			"name":           "https",
			"attachedRoutes": float64(1),
		}},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "gw", "ns")
	require.NoError(t, err)
	assert.Equal(t, int64(8443), meta.Port)
}

func TestExtractMetadata_TLSListener(t *testing.T) {
	gw := makeGateway(
		[]any{map[string]any{
			"name":     "tls",
			"protocol": "TLS",
			"port":     int64(443),
			"hostname": "gw.example.com",
		}},
		[]any{readyStatus("tls", int64(2))},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "gw", "ns")
	require.NoError(t, err)
	assert.Equal(t, "https", meta.Protocol)
}
