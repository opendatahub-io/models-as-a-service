package cache_test

import (
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cache"
)

var testLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

func makeTenant(name, gwName string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "ai-tenants"},
		"spec":     map[string]any{},
	}}
	if gwName != "" {
		u.Object["spec"] = map[string]any{
			"gateway": map[string]any{"name": gwName},
		}
	}
	return u
}

func makeGatewayObj(name string, specListeners, statusListeners, addresses []any) unstructured.Unstructured {
	status := map[string]any{
		"listeners": statusListeners,
	}
	if addresses != nil {
		status["addresses"] = addresses
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "gw-ns"},
		"spec": map[string]any{
			"listeners": specListeners,
		},
		"status": status,
	}}
}

func specListener(protocol, hostname string, port int64) map[string]any {
	l := map[string]any{
		"name":     protocol,
		"protocol": protocol,
		"port":     port,
	}
	if hostname != "" {
		l["hostname"] = hostname
	}
	return l
}

func statusListenerReady(name string, routes int64) map[string]any {
	return map[string]any{
		"name":           name,
		"attachedRoutes": routes,
	}
}

func TestBuildTenantInfos_SingleTenantWithGateway(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "team-a-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("team-a-gw",
			[]any{specListener("HTTPS", "team-a.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "team-a", result[0].Name)
	assert.Equal(t, "team-a-gw", result[0].Gateway.Name)
	assert.Equal(t, "gw-ns", result[0].Gateway.Namespace)
	assert.Equal(t, "https", result[0].Gateway.Protocol)
	assert.Equal(t, "https://team-a.example.com", result[0].Gateway.ExternalURL)
	assert.Equal(t, int64(443), result[0].Gateway.Port)
}

func TestBuildTenantInfos_MultipleTenants(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "team-a-gw"),
		makeTenant("team-b", "team-b-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("team-a-gw",
			[]any{specListener("HTTPS", "a.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			nil,
		),
		makeGatewayObj("team-b-gw",
			[]any{specListener("HTTP", "b.example.com", 80)},
			[]any{statusListenerReady("HTTP", 2)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 2)
	assert.Equal(t, "team-a", result[0].Name)
	assert.Equal(t, "https://a.example.com", result[0].Gateway.ExternalURL)
	assert.Equal(t, "team-b", result[1].Name)
	assert.Equal(t, "http://b.example.com", result[1].Gateway.ExternalURL)
}

func TestBuildTenantInfos_GatewayNotFound(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "missing-gw"),
	}

	result := cache.BuildTenantInfos(tenants, nil, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "team-a", result[0].Name)
	assert.Equal(t, "missing-gw", result[0].Gateway.Name)
	assert.Equal(t, "gw-ns", result[0].Gateway.Namespace)
	assert.Empty(t, result[0].Gateway.ExternalURL)
	assert.Empty(t, result[0].Gateway.Protocol)
	assert.Zero(t, result[0].Gateway.Port)
}

func TestBuildTenantInfos_GatewayWithoutStatus(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "broken-gw"),
	}
	gateways := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": "broken-gw", "namespace": "gw-ns"},
			"spec":     map[string]any{"listeners": []any{specListener("HTTPS", "a.example.com", 443)}},
		}},
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "broken-gw", result[0].Gateway.Name)
	assert.Empty(t, result[0].Gateway.ExternalURL, "should degrade gracefully without status")
}

func TestBuildTenantInfos_TenantWithoutGatewayRef(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("default-gw-tenant", ""),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("default-gw-tenant",
			[]any{specListener("HTTPS", "default.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "default-gw-tenant", result[0].Gateway.Name,
		"should fall back to tenant name when spec.gateway.name is empty")
	assert.Equal(t, "https://default.example.com", result[0].Gateway.ExternalURL)
}

func TestBuildTenantInfos_NoTenants(t *testing.T) {
	result := cache.BuildTenantInfos(nil, nil, "gw-ns", testLog)
	assert.Empty(t, result)
}

func TestBuildTenantInfos_SharedGateway(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "shared-gw"),
		makeTenant("team-b", "shared-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("shared-gw",
			[]any{specListener("HTTPS", "shared.example.com", 443)},
			[]any{statusListenerReady("HTTPS", 3)},
			nil,
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 2)
	assert.Equal(t, "https://shared.example.com", result[0].Gateway.ExternalURL)
	assert.Equal(t, "https://shared.example.com", result[1].Gateway.ExternalURL)
}

func TestBuildTenantInfos_GatewayWithStatusAddresses(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "addr-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("addr-gw",
			[]any{specListener("HTTPS", "", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			[]any{map[string]any{"value": "lb.example.com", "type": "Hostname"}},
		),
	}

	result := cache.BuildTenantInfos(tenants, gateways, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "https://lb.example.com", result[0].Gateway.ExternalURL)
}

func TestInformerCache_Synced(t *testing.T) {
	ic, err := cache.NewInformerCache(cache.InformerCacheOptions{
		RestConfig:       &rest.Config{Host: "https://fake"},
		TenantNamespace:  "ai-tenants",
		GatewayNamespace: "openshift-ingress",
	})
	require.NoError(t, err)
	assert.False(t, ic.Synced(), "should not be synced before Start")
}

func TestInformerCache_List_BeforeStart(t *testing.T) {
	ic, err := cache.NewInformerCache(cache.InformerCacheOptions{
		RestConfig:       &rest.Config{Host: "https://fake"},
		TenantNamespace:  "ai-tenants",
		GatewayNamespace: "openshift-ingress",
	})
	require.NoError(t, err)
	assert.Nil(t, ic.List())
}

func TestInformerCache_ImplementsTenantCache(t *testing.T) {
	var _ cache.TenantCache = (*cache.InformerCache)(nil)
}

func TestNewInformerCache_Validation(t *testing.T) {
	tests := []struct {
		name string
		opts cache.InformerCacheOptions
	}{
		{"no rest config", cache.InformerCacheOptions{TenantNamespace: "a", GatewayNamespace: "b"}},
		{"no tenant namespace", cache.InformerCacheOptions{RestConfig: &rest.Config{}, GatewayNamespace: "b"}},
		{"no gateway namespace", cache.InformerCacheOptions{RestConfig: &rest.Config{}, TenantNamespace: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cache.NewInformerCache(tt.opts)
			assert.Error(t, err)
		})
	}
}
