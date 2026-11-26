package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

func routeObj(host, svc string, tls, admitted bool) unstructured.Unstructured {
	return routeObjWithSpecHost(host, host, svc, tls, admitted, "", nil)
}

// routeObjWithPort builds an admitted, host-only, TLS Route whose backend
// targets the given Service port (an IntOrString value: number or name).
func routeObjWithPort(host, svc string, targetPort any) unstructured.Unstructured {
	route := routeObj(host, svc, true, true)
	spec, ok := route.Object["spec"].(map[string]any)
	if !ok {
		panic("route spec is not a map")
	}
	spec["port"] = map[string]any{"targetPort": targetPort}
	return route
}

// routeObjWithSpecHost builds a Route whose spec.host may differ from the host
// published by the admitting status.ingress entry, with an optional path and
// backend weight.
func routeObjWithSpecHost(specHost, ingressHost, svc string, tls, admitted bool, path string, weight *int64) unstructured.Unstructured {
	route := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"host": specHost,
			"to":   map[string]any{"kind": "Service", "name": svc},
		},
	}}
	if path != "" {
		spec, ok := route.Object["spec"].(map[string]any)
		if !ok {
			panic("route spec is not a map")
		}
		spec["path"] = path
	}
	if weight != nil {
		spec, ok := route.Object["spec"].(map[string]any)
		if !ok {
			panic("route spec is not a map")
		}
		to, ok := spec["to"].(map[string]any)
		if !ok {
			panic("route to is not a map")
		}
		to["weight"] = *weight
	}
	if tls {
		spec, ok := route.Object["spec"].(map[string]any)
		if !ok {
			panic("route spec is not a map")
		}
		spec["tls"] = map[string]any{"termination": "edge"}
	}
	statusValue := "False"
	if admitted {
		statusValue = "True"
	}
	route.Object["status"] = map[string]any{
		"ingress": []any{
			map[string]any{
				"host": ingressHost,
				"conditions": []any{
					map[string]any{"type": "Admitted", "status": statusValue},
				},
			},
		},
	}
	return route
}

func TestExtractMetadata_HTTPS(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(1))},
		nil,
	)

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
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

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
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

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
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

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
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

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "https://lb.example.com", meta.ExternalURL)
}

func TestExtractMetadata_RejectsInternalHostname(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-svc.default.svc.cluster.local"}},
	)

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal service name")
}

func TestExtractMetadata_NoExternalHostname(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		nil,
	)

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine external hostname")
}

func TestExtractMetadata_NoSpec(t *testing.T) {
	gw := map[string]any{"status": map[string]any{}}
	_, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	assert.Error(t, err)
}

func TestExtractMetadata_NoStatus(t *testing.T) {
	gw := map[string]any{"spec": map[string]any{"listeners": []any{}}}
	_, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	assert.Error(t, err)
}

func TestExtractMetadata_NoListeners(t *testing.T) {
	gw := makeGateway(
		[]any{},
		[]any{readyStatus("https", int64(1))},
		nil,
	)
	_, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	assert.Error(t, err)
}

func TestExtractMetadata_NoReadyListeners(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(0))},
		nil,
	)

	_, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ready listeners")
}

func TestExtractMetadata_NoReadyListenersWithAddresses(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("gw.example.com", int64(443))},
		[]any{readyStatus("https", int64(0))},
		[]any{map[string]any{"value": "lb.example.com", "type": "Hostname"}},
	)

	_, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
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

	meta, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(8443), meta.Port)
}

func TestExtractMetadata_RouteHostFallback(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw-svc.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-svc": {
			Host:   "maas.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}
	gwServices := gateway.GatewayServices{"my-gw-svc": {{Port: 443}}}

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.NoError(t, err)
	assert.Equal(t, "https://maas.apps.example.com", meta.ExternalURL)
}

func TestExtractMetadata_RouteHostFallbackUsesRouteSchemeAndPort(t *testing.T) {
	gw := makeGateway(
		[]any{httpListener("", int64(80))},
		[]any{readyStatus("http", int64(1))},
		[]any{map[string]any{"value": "my-gw-svc.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-svc": {
			Host:   "maas.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}
	gwServices := gateway.GatewayServices{"my-gw-svc": {{Port: 80}, {Port: 443}}}

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.NoError(t, err)
	assert.Equal(t, "https", meta.Protocol)
	assert.Equal(t, int64(443), meta.Port)
	assert.Equal(t, "https://maas.apps.example.com", meta.ExternalURL)
}

func TestExtractMetadata_RouteHostFallbackNoMatch(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw-svc.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"other-svc": {
			Host:   "other.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}
	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal service name")
}

// TestExtractMetadata_RouteHostFallbackMatchesSelectorService covers the
// selector-based matching: the Route targets a Service whose name differs
// from the Gateway's internal hostname prefix, but which selects the Gateway
// via the gateway.networking.k8s.io/gateway-name selector label.
func TestExtractMetadata_RouteHostFallbackMatchesSelectorService(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"custom-edge-svc": {
			Host:   "maas.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}
	gwServices := gateway.GatewayServices{"custom-edge-svc": {{Port: 443}}}

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.NoError(t, err)
	assert.Equal(t, "https://maas.apps.example.com", meta.ExternalURL)
}

// TestExtractMetadata_RouteFallbackIgnoresServiceWithoutSelector ensures that
// a Route targeting a Service named like the internal hostname prefix is NOT
// matched unless that Service actually selects the Gateway.
func TestExtractMetadata_RouteFallbackIgnoresServiceWithoutSelector(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw-svc.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-svc": {
			Host:   "unrelated.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}
	gwServices := gateway.GatewayServices{"some-other-svc": {{Port: 443}}}

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal service name")
}

// TestExtractMetadata_RouteFallbackMultipleServicesDeterministic: when several
// selector-matching Services have Route endpoints, the winner is chosen
// deterministically — HTTPS preferred, then lexicographically by host —
// independent of map iteration order.
func TestExtractMetadata_RouteFallbackMultipleServicesDeterministic(t *testing.T) {
	gw := makeGateway(
		[]any{httpListener("", int64(8080))},
		[]any{readyStatus("http", int64(1))},
		[]any{map[string]any{"value": "my-gw.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"svc-a": {Host: "zeta.apps.example.com", Scheme: "https", Port: 443},
		"svc-b": {Host: "alpha.apps.example.com", Scheme: "https", Port: 443},
		"svc-c": {Host: "aaa.apps.example.com", Scheme: "http", Port: 80},
	}
	gwServices := gateway.GatewayServices{
		"svc-a": {{Port: 8080}},
		"svc-b": {{Port: 8080}},
		"svc-c": {{Port: 8080}},
	}

	for range 50 {
		meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
		require.NoError(t, err)
		assert.Equal(t, "https", meta.Protocol, "HTTPS endpoint must win over HTTP")
		assert.Equal(t, "https://alpha.apps.example.com", meta.ExternalURL,
			"lexicographically smallest HTTPS host must win")
	}
}

func TestBuildRouteHostMap(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObj("maas.apps.example.com", "maas-gw-svc", true, true),
		routeObj("other.apps.example.com", "other-svc", false, true),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "maas.apps.example.com", m["maas-gw-svc"].Host)
	assert.Equal(t, "https", m["maas-gw-svc"].Scheme)
	assert.Equal(t, int64(443), m["maas-gw-svc"].Port)
	assert.Equal(t, "other.apps.example.com", m["other-svc"].Host)
	assert.Equal(t, "http", m["other-svc"].Scheme)
	assert.Equal(t, int64(80), m["other-svc"].Port)
	assert.Len(t, m, 2)
}

func TestBuildRouteHostMap_SkipsNonAdmittedRoutes(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObj("rejected.apps.example.com", "shared-svc", true, false),
		routeObj("accepted.apps.example.com", "shared-svc", true, true),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "accepted.apps.example.com", m["shared-svc"].Host)
}

func TestBuildRouteHostMap_PrefersTLSEndpointForService(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObj("http.apps.example.com", "shared-svc", false, true),
		routeObj("https.apps.example.com", "shared-svc", true, true),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "https.apps.example.com", m["shared-svc"].Host)
	assert.Equal(t, "https", m["shared-svc"].Scheme)
	assert.Equal(t, int64(443), m["shared-svc"].Port)
}

func TestBuildRouteHostMap_Empty(t *testing.T) {
	m := gateway.BuildRouteHostMap(nil)
	assert.Empty(t, m)
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

	meta, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "https", meta.Protocol)
}

func TestExtractMetadata_IPv6AddressBracketed(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(8443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "fd00::1"}},
	)

	meta, err := gateway.ExtractMetadata(gw, "gw", "ns", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "https://[fd00::1]:8443", meta.ExternalURL)
	assert.Equal(t, int64(8443), meta.Port)
}

func TestExtractMetadata_RouteHostFallbackForSvcSuffix(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw-svc.gw-ns.svc"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-svc": {
			Host:   "maas.apps.example.com",
			Scheme: "https",
			Port:   443,
		},
	}

	gwServices := gateway.GatewayServices{"my-gw-svc": {{Port: 443}}}

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.NoError(t, err)
	assert.Equal(t, "https://maas.apps.example.com", meta.ExternalURL)
}

func TestBuildRouteHostMap_IgnoresPathBasedRoutes(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObjWithSpecHost("maas.apps.example.com", "maas.apps.example.com", "maas-gw-svc", true, true, "/foo", nil),
		routeObjWithSpecHost("root.apps.example.com", "root.apps.example.com", "maas-gw-svc", true, true, "", nil),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "root.apps.example.com", m["maas-gw-svc"].Host)
}

func TestBuildRouteHostMap_UsesAdmittedIngressHost(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObjWithSpecHost("wrong.apps.example.com", "sharded.apps.example.com", "maas-gw-svc", true, true, "", nil),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "sharded.apps.example.com", m["maas-gw-svc"].Host)
}

func TestBuildRouteHostMap_SkipsDisabledBackends(t *testing.T) {
	zero := int64(0)
	routes := []unstructured.Unstructured{
		routeObjWithSpecHost("disabled.apps.example.com", "disabled.apps.example.com", "maas-gw-svc", true, true, "", &zero),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.NotContains(t, m, "maas-gw-svc")
}

func TestBuildRouteHostMap_DeterministicTieBreak(t *testing.T) {
	// Two plain-HTTP Routes for the same Service: the lexicographically smaller
	// host must win regardless of the order the informer delivers them in.
	forward := []unstructured.Unstructured{
		routeObj("zeta.apps.example.com", "shared-svc", false, true),
		routeObj("alpha.apps.example.com", "shared-svc", false, true),
	}
	reverse := []unstructured.Unstructured{
		routeObj("alpha.apps.example.com", "shared-svc", false, true),
		routeObj("zeta.apps.example.com", "shared-svc", false, true),
	}

	forwardMap := gateway.BuildRouteHostMap(forward)
	reverseMap := gateway.BuildRouteHostMap(reverse)
	assert.Equal(t, "alpha.apps.example.com", forwardMap["shared-svc"].Host)
	assert.Equal(t, forwardMap["shared-svc"], reverseMap["shared-svc"])
}

func makeServiceObj(name string, selector map[string]any, ports ...any) unstructured.Unstructured {
	svc := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "gw-ns"},
		"spec":     map[string]any{},
	}}
	spec, ok := svc.Object["spec"].(map[string]any)
	if !ok {
		panic("service spec is not a map")
	}
	if selector != nil {
		spec["selector"] = selector
	}
	if len(ports) > 0 {
		spec["ports"] = ports
	}
	return svc
}

func TestSelectGatewayServices(t *testing.T) {
	gwLabel := map[string]any{"gateway.networking.k8s.io/gateway-name": "my-gw"}
	services := []unstructured.Unstructured{
		makeServiceObj("my-gw", gwLabel,
			map[string]any{"name": "https", "port": int64(443)},
			map[string]any{"name": "metrics", "port": int64(8082)},
		),
		makeServiceObj("custom-edge-svc", gwLabel,
			map[string]any{"port": int64(443)},
		),
		makeServiceObj("other-gw-svc", map[string]any{"gateway.networking.k8s.io/gateway-name": "other-gw"}),
		makeServiceObj("no-selector-svc", nil),
		makeServiceObj("wrong-label-svc", map[string]any{"app": "my-gw"}),
	}

	selected := gateway.SelectGatewayServices(services, "my-gw")
	assert.Equal(t, gateway.GatewayServices{
		"my-gw": {
			{Name: "https", Port: 443},
			{Name: "metrics", Port: 8082},
		},
		"custom-edge-svc": {
			{Port: 443},
		},
	}, selected)
}

func TestSelectGatewayServices_Empty(t *testing.T) {
	assert.Empty(t, gateway.SelectGatewayServices(nil, "my-gw"))
}

// TestBuildRouteHostMap_RetainsTargetPort ensures the Route's backend port is
// carried through for listener validation: numeric, numeric-string, and named
// IntOrString forms must all be extracted.
func TestBuildRouteHostMap_RetainsTargetPort(t *testing.T) {
	routes := []unstructured.Unstructured{
		routeObjWithPort("named.apps.example.com", "named-svc", "https"),
		routeObjWithPort("numeric.apps.example.com", "numeric-svc", int64(443)),
		routeObjWithPort("numeric-string.apps.example.com", "string-svc", "8080"),
	}

	m := gateway.BuildRouteHostMap(routes)
	assert.Equal(t, "https", m["named-svc"].TargetPortName)
	assert.Equal(t, int64(0), m["named-svc"].TargetPort)
	assert.Equal(t, int64(443), m["numeric-svc"].TargetPort)
	assert.Empty(t, m["numeric-svc"].TargetPortName)
	assert.Equal(t, int64(8080), m["string-svc"].TargetPort)
	assert.Empty(t, m["string-svc"].TargetPortName)
}

// TestExtractMetadata_RouteFallbackRejectsNonListenerPort covers the backend
// port validation: a Service that selects the Gateway pods can expose
// non-listener ports such as metrics, and a Route targeting such a port must
// not provide the Gateway's external URL.
func TestExtractMetadata_RouteFallbackRejectsNonListenerPort(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-metrics": {Host: "metrics.apps.example.com", Scheme: "https", Port: 443},
	}
	gwServices := gateway.GatewayServices{"my-gw-metrics": {{Name: "metrics", Port: 8082}}}

	_, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal service name")
}

// TestExtractMetadata_RouteFallbackPrefersListenerBackedRoute: when both a
// metrics-backed and a listener-backed Route exist, only the listener-backed
// one is a candidate, even when its host would lose the deterministic tie-break.
func TestExtractMetadata_RouteFallbackPrefersListenerBackedRoute(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw.gw-ns.svc.cluster.local"}},
	)

	routeHosts := map[string]gateway.RouteHostInfo{
		"my-gw-metrics": {Host: "aaa.metrics.apps.example.com", Scheme: "https", Port: 443},
		"my-gw-svc":     {Host: "zzz.apps.example.com", Scheme: "https", Port: 443},
	}
	gwServices := gateway.GatewayServices{
		"my-gw-metrics": {{Name: "metrics", Port: 8082}},
		"my-gw-svc":     {{Name: "https", Port: 443}},
	}

	meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
	require.NoError(t, err)
	assert.Equal(t, "https://zzz.apps.example.com", meta.ExternalURL)
}

// TestExtractMetadata_RouteFallbackTargetPortValidation exercises the Route's
// spec.port.targetPort against a Service exposing both a listener port and a
// metrics port.
func TestExtractMetadata_RouteFallbackTargetPortValidation(t *testing.T) {
	gw := makeGateway(
		[]any{httpsListener("", int64(443))},
		[]any{readyStatus("https", int64(1))},
		[]any{map[string]any{"value": "my-gw.gw-ns.svc.cluster.local"}},
	)
	svcPorts := []gateway.ServicePort{{Name: "https", Port: 443}, {Name: "metrics", Port: 8082}}

	tests := []struct {
		name           string
		targetPort     int64
		targetPortName string
		wantURL        string
		wantErr        string
	}{
		{name: "named port reaching listener", targetPortName: "https", wantURL: "https://maas.apps.example.com"},
		{name: "named metrics port rejected", targetPortName: "metrics", wantErr: "internal service name"},
		{name: "numeric port reaching listener", targetPort: 443, wantURL: "https://maas.apps.example.com"},
		{name: "numeric metrics port rejected", targetPort: 8082, wantErr: "internal service name"},
		{name: "numeric port not exposed by service rejected", targetPort: 9090, wantErr: "internal service name"},
		{name: "unspecified port resolves to any listener port", wantURL: "https://maas.apps.example.com"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			routeHosts := map[string]gateway.RouteHostInfo{
				"my-gw-svc": {
					Host:           "maas.apps.example.com",
					Scheme:         "https",
					Port:           443,
					TargetPort:     test.targetPort,
					TargetPortName: test.targetPortName,
				},
			}
			gwServices := gateway.GatewayServices{"my-gw-svc": svcPorts}

			meta, err := gateway.ExtractMetadata(gw, "my-gw", "gw-ns", routeHosts, gwServices)
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantURL, meta.ExternalURL)
		})
	}
}
