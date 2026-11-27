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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

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

	result := cache.BuildTenantInfos(tenants, nil, nil, nil, "gw-ns", testLog)

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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "default-gw-tenant", result[0].Gateway.Name,
		"should fall back to tenant name when spec.gateway.name is empty")
	assert.Equal(t, "https://default.example.com", result[0].Gateway.ExternalURL)
}

func TestBuildTenantInfos_NoTenants(t *testing.T) {
	result := cache.BuildTenantInfos(nil, nil, nil, nil, "gw-ns", testLog)
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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

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

	result := cache.BuildTenantInfos(tenants, gateways, nil, nil, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "https://lb.example.com", result[0].Gateway.ExternalURL)
}

func makeServiceObj(name string, selector map[string]any, ports ...any) unstructured.Unstructured {
	spec := map[string]any{}
	if selector != nil {
		spec["selector"] = selector
	}
	if len(ports) > 0 {
		spec["ports"] = ports
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "gw-ns"},
		"spec":     spec,
	}}
}

func makeRouteObj(host, svcName string) unstructured.Unstructured {
	route := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"host": host,
			"to":   map[string]any{"kind": "Service", "name": svcName},
		},
	}}
	spec, ok := route.Object["spec"].(map[string]any)
	if !ok {
		panic("route spec is not a map")
	}
	spec["tls"] = map[string]any{"termination": "edge"}
	route.Object["status"] = map[string]any{
		"ingress": []any{
			map[string]any{
				"host": host,
				"conditions": []any{
					map[string]any{"type": "Admitted", "status": "True"},
				},
			},
		},
	}
	return route
}

// makeRouteObjWithPort builds an admitted, host-only, TLS Route whose backend
// targets the given Service port.
func makeRouteObjWithPort(host, svcName string, targetPort int64) unstructured.Unstructured {
	route := makeRouteObj(host, svcName)
	spec, ok := route.Object["spec"].(map[string]any)
	if !ok {
		panic("route spec is not a map")
	}
	spec["port"] = map[string]any{"targetPort": targetPort}
	return route
}

func TestBuildTenantInfos_RouteHostFallback(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "internal-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("internal-gw",
			[]any{specListener("HTTPS", "", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			[]any{map[string]any{"value": "internal-gw-svc.gw-ns.svc.cluster.local"}},
		),
	}
	routes := []unstructured.Unstructured{
		makeRouteObj("maas.apps.example.com", "internal-gw-svc"),
	}
	services := []unstructured.Unstructured{
		makeServiceObj("internal-gw-svc", map[string]any{
			"gateway.networking.k8s.io/gateway-name": "internal-gw",
		}, map[string]any{"name": "https", "port": int64(443)}),
	}

	result := cache.BuildTenantInfos(tenants, gateways, routes, services, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "https://maas.apps.example.com", result[0].Gateway.ExternalURL)
}

// TestBuildTenantInfos_RouteFallbackSelectorMatching covers defect 5: the
// Route targets a Service whose name differs from the Gateway's internal
// hostname prefix, matched via the gateway-name selector label instead.
func TestBuildTenantInfos_RouteFallbackSelectorMatching(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "internal-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("internal-gw",
			[]any{specListener("HTTPS", "", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			[]any{map[string]any{"value": "internal-gw.gw-ns.svc.cluster.local"}},
		),
	}
	routes := []unstructured.Unstructured{
		makeRouteObj("maas.apps.example.com", "custom-edge-svc"),
	}
	services := []unstructured.Unstructured{
		makeServiceObj("custom-edge-svc", map[string]any{
			"gateway.networking.k8s.io/gateway-name": "internal-gw",
		}, map[string]any{"port": int64(443)}),
	}

	result := cache.BuildTenantInfos(tenants, gateways, routes, services, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Equal(t, "https://maas.apps.example.com", result[0].Gateway.ExternalURL)
}

// TestBuildTenantInfos_RouteFallbackRejectsMetricsPort: a Route targeting a
// Service that selects the Gateway pods but only exposes a non-listener port
// (e.g. metrics) must not provide the Gateway's external URL.
func TestBuildTenantInfos_RouteFallbackRejectsMetricsPort(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "internal-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("internal-gw",
			[]any{specListener("HTTPS", "", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			[]any{map[string]any{"value": "internal-gw.gw-ns.svc.cluster.local"}},
		),
	}
	routes := []unstructured.Unstructured{
		makeRouteObjWithPort("metrics.apps.example.com", "internal-gw-metrics", int64(8082)),
	}
	services := []unstructured.Unstructured{
		makeServiceObj("internal-gw-metrics", map[string]any{
			"gateway.networking.k8s.io/gateway-name": "internal-gw",
		}, map[string]any{"name": "metrics", "port": int64(8082)}),
	}

	result := cache.BuildTenantInfos(tenants, gateways, routes, services, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Empty(t, result[0].Gateway.ExternalURL,
		"a Route to a non-listener port must not provide the external URL")
}

// TestBuildTenantInfos_RouteFallbackNoSelectorService: a Route targeting a
// Service that does not select the Gateway must not be used, even when its
// name matches the internal hostname prefix.
func TestBuildTenantInfos_RouteFallbackNoSelectorService(t *testing.T) {
	tenants := []unstructured.Unstructured{
		makeTenant("team-a", "internal-gw"),
	}
	gateways := []unstructured.Unstructured{
		makeGatewayObj("internal-gw",
			[]any{specListener("HTTPS", "", 443)},
			[]any{statusListenerReady("HTTPS", 1)},
			[]any{map[string]any{"value": "internal-gw-svc.gw-ns.svc.cluster.local"}},
		),
	}
	routes := []unstructured.Unstructured{
		makeRouteObj("unrelated.apps.example.com", "internal-gw-svc"),
	}
	services := []unstructured.Unstructured{
		makeServiceObj("internal-gw-svc", map[string]any{
			"gateway.networking.k8s.io/gateway-name": "some-other-gw",
		}),
	}

	result := cache.BuildTenantInfos(tenants, gateways, routes, services, "gw-ns", testLog)

	require.Len(t, result, 1)
	assert.Empty(t, result[0].Gateway.ExternalURL,
		"partial data must be returned when no selector-matching Route exists")
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
