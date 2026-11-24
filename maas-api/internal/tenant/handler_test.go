//nolint:testpackage // tests access unexported extractGatewayMetadata method
package tenant

import (
	"context"
	"errors"
	"testing"

	routv1 "github.com/openshift/api/route/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

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
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

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
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.NoError(t, err)
	assert.Equal(t, "https://maas.example.com:8443", metadata.ExternalURL)
	assert.Equal(t, int64(8443), metadata.Port)
}

func TestExtractGatewayMetadata_HTTPListener(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.NoError(t, err)
	assert.Equal(t, "http://maas.example.com", metadata.ExternalURL)
	assert.Equal(t, "http", metadata.Protocol)
}

func TestExtractGatewayMetadata_NoListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

	gateway := map[string]any{
		"spec": map[string]any{
			"listeners": []any{},
		},
		"status": map[string]any{
			"listeners": []any{},
		},
	}

	_, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no listeners")
}

func TestExtractGatewayMetadata_NoReadyListeners(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	_, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not determine external hostname")
}

func TestExtractGatewayMetadata_FallbackToStatusAddresses(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.NoError(t, err)
	assert.Equal(t, "https://gateway.fallback.example.com", metadata.ExternalURL)
	assert.Equal(t, "https", metadata.Protocol)
	assert.Equal(t, int64(443), metadata.Port)
}

func TestExtractGatewayMetadata_RejectsInternalServiceName(t *testing.T) {
	log := logger.New(false)
	handler := NewHandler(log, nil, nil, "test-tenant", "test-gateway", "test-ns")

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

	_, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured with an external hostname")
	assert.Contains(t, err.Error(), "internal service name")
}

func TestExtractGatewayMetadata_FallsBackToOpenShiftRoute(t *testing.T) {
	handler := NewHandler(
		logger.New(false),
		fakeClusterWithGatewayRoute(t, true),
		fakeRouteDiscovery(),
		"test-tenant",
		"test-gateway",
		"test-ns",
	)
	gateway := internalGatewayWithClusterLocalAddress()

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.NoError(t, err)
	assert.Equal(t, "https", metadata.Protocol)
	assert.Equal(t, "https://maas.apps.example.com", metadata.ExternalURL)
	assert.Equal(t, int64(443), metadata.Port)
}

func TestExtractGatewayMetadata_OpenShiftRouteHTTPFallback(t *testing.T) {
	handler := NewHandler(
		logger.New(false),
		fakeClusterWithGatewayRoute(t, false),
		fakeRouteDiscovery(),
		"test-tenant",
		"test-gateway",
		"test-ns",
	)
	gateway := internalGatewayWithClusterLocalAddress()

	metadata, err := handler.extractGatewayMetadata(context.Background(), gateway)

	require.NoError(t, err)
	assert.Equal(t, "http", metadata.Protocol)
	assert.Equal(t, "http://maas.apps.example.com", metadata.ExternalURL)
	assert.Equal(t, int64(80), metadata.Port)
}

func TestExtractGatewayMetadata_SkipsRouteFallbackWhenRouteAPIUnavailable(t *testing.T) {
	handler := NewHandler(
		logger.New(false),
		fakeClusterWithGatewayRoute(t, true),
		&fakeServerGroups{},
		"test-tenant",
		"test-gateway",
		"test-ns",
	)

	_, err := handler.extractGatewayMetadata(context.Background(), internalGatewayWithClusterLocalAddress())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured with an external hostname or compatible OpenShift Route")
	assert.NotContains(t, err.Error(), "failed to discover an OpenShift Route")
}

func TestExtractGatewayMetadata_PreservesForbiddenRouteAPIError(t *testing.T) {
	client := fakeClusterWithGatewayRoute(t, true)
	client.PrependReactor("list", "routes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: routeAPIGroup, Resource: "routes"},
			"",
			errors.New("denied"),
		)
	})
	handler := NewHandler(logger.New(false), client, fakeRouteDiscovery(), "test-tenant", "test-gateway", "test-ns")

	_, err := handler.extractGatewayMetadata(context.Background(), internalGatewayWithClusterLocalAddress())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to discover an OpenShift Route")
	assert.Contains(t, err.Error(), "forbidden")
}

func TestExtractGatewayMetadata_PrefersHTTPSOpenShiftRoute(t *testing.T) {
	httpsRoute := gatewayRoute(true)
	httpsRoute.Name = "test-gateway-route-https"
	httpsRoute.Status.Ingress[0].Host = "zz-maas.apps.example.com"
	client := fakeClusterWithGatewayRoutes(t, gatewayRoute(false), httpsRoute)

	handler := NewHandler(logger.New(false), client, fakeRouteDiscovery(), "test-tenant", "test-gateway", "test-ns")

	metadata, err := handler.extractGatewayMetadata(context.Background(), internalGatewayWithClusterLocalAddress())

	require.NoError(t, err)
	assert.Equal(t, "https://zz-maas.apps.example.com", metadata.ExternalURL)
}

func TestExtractGatewayMetadata_IgnoresDisabledOpenShiftRouteBackend(t *testing.T) {
	disabledRoute := gatewayRoute(true)
	disabledRoute.Name = "test-gateway-route-disabled"
	disabledRoute.Status.Ingress[0].Host = "a-maas.apps.example.com"
	disabledWeight := int32(0)
	disabledRoute.Spec.To.Weight = &disabledWeight

	workingRoute := gatewayRoute(true)
	workingRoute.Name = "test-gateway-route-working"
	workingRoute.Status.Ingress[0].Host = "z-maas.apps.example.com"
	client := fakeClusterWithGatewayRoutes(t, disabledRoute, workingRoute)

	handler := NewHandler(logger.New(false), client, fakeRouteDiscovery(), "test-tenant", "test-gateway", "test-ns")

	metadata, err := handler.extractGatewayMetadata(context.Background(), internalGatewayWithClusterLocalAddress())

	require.NoError(t, err)
	assert.Equal(t, "https://z-maas.apps.example.com", metadata.ExternalURL)
}

func TestExtractGatewayMetadata_PreservesPrivateIPAddressWhenRouteAPIUnavailable(t *testing.T) {
	handler := NewHandler(logger.New(false), nil, nil, "test-tenant", "test-gateway", "test-ns")

	metadata, err := handler.extractGatewayMetadata(context.Background(), internalGatewayWithAddress("10.20.30.40"))

	require.NoError(t, err)
	assert.Equal(t, "https://10.20.30.40", metadata.ExternalURL)
}

func TestExtractGatewayMetadata_BracketsIPv6Addresses(t *testing.T) {
	tests := []struct {
		port     int64
		expected string
	}{
		{port: int64(443), expected: "https://[fd00::1]"},
		{port: int64(8443), expected: "https://[fd00::1]:8443"},
	}

	for _, test := range tests {
		handler := NewHandler(logger.New(false), nil, nil, "test-tenant", "test-gateway", "test-ns")

		metadata, err := handler.extractGatewayMetadata(
			context.Background(),
			internalGatewayWithAddressAndPort("fd00::1", test.port),
		)

		require.NoError(t, err)
		assert.Equal(t, test.expected, metadata.ExternalURL)
		assert.Equal(t, test.port, metadata.Port)
	}
}

func TestIsLocalHostname(t *testing.T) {
	tests := []struct {
		hostname string
		expected bool
	}{
		{hostname: "service.namespace.svc.cluster.local", expected: true},
		{hostname: "service.namespace.svc", expected: true},
		{hostname: "service.namespace.local", expected: true},
		{hostname: "service.internal", expected: true},
		{hostname: "localhost", expected: true},
		{hostname: "127.0.0.1", expected: true},
		{hostname: "10.0.0.1", expected: false},
		{hostname: "192.168.1.1", expected: false},
		{hostname: "169.254.1.1", expected: true},
		{hostname: "maas.apps.example.com", expected: false},
		{hostname: "clusterlocal.example.com", expected: false},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, isLocalHostname(test.hostname), test.hostname)
	}
}

func internalGatewayWithClusterLocalAddress() map[string]any {
	return internalGatewayWithAddress("test-gateway.test-ns.svc.cluster.local")
}

func internalGatewayWithAddress(address string) map[string]any {
	return internalGatewayWithAddressAndPort(address, int64(443))
}

func internalGatewayWithAddressAndPort(address string, port int64) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"listeners": []any{
				map[string]any{"name": "https", "port": port, "protocol": "HTTPS"},
			},
		},
		"status": map[string]any{
			"addresses": []any{
				map[string]any{
					"type":  "Hostname",
					"value": address,
				},
			},
			"listeners": []any{
				map[string]any{"name": "https", "attachedRoutes": int64(1)},
			},
		},
	}
}

func fakeClusterWithGatewayRoute(t *testing.T, tlsEnabled bool) *dynamicfake.FakeDynamicClient {
	t.Helper()

	return fakeClusterWithGatewayRoutes(t, gatewayRoute(tlsEnabled))
}

func fakeClusterWithGatewayRoutes(t *testing.T, routes ...*routv1.Route) *dynamicfake.FakeDynamicClient {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, routv1.AddToScheme(scheme))

	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-service",
			Namespace: "test-ns",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{gatewayNameLabel: "test-gateway"},
		},
	}

	objects := make([]runtime.Object, 0, len(routes)+1)
	objects = append(objects, service)
	for _, route := range routes {
		objects = append(objects, route)
	}

	return dynamicfake.NewSimpleDynamicClient(scheme, objects...)
}

func gatewayRoute(tlsEnabled bool) *routv1.Route {
	route := &routv1.Route{
		TypeMeta: metav1.TypeMeta{APIVersion: "route.openshift.io/v1", Kind: "Route"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-route",
			Namespace: "test-ns",
		},
		Spec: routv1.RouteSpec{
			To: routv1.RouteTargetReference{
				Kind: "Service",
				Name: "test-gateway-service",
			},
		},
		Status: routv1.RouteStatus{
			Ingress: []routv1.RouteIngress{
				{
					Host: "maas.apps.example.com",
					Conditions: []routv1.RouteIngressCondition{
						{
							Type:   routv1.RouteAdmitted,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
		},
	}
	if tlsEnabled {
		route.Spec.TLS = &routv1.TLSConfig{Termination: routv1.TLSTerminationEdge}
	}

	return route
}

type fakeServerGroups struct {
	groups []metav1.APIGroup
}

func (f *fakeServerGroups) ServerGroupsWithContext(_ context.Context) (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{Groups: f.groups}, nil
}

func fakeRouteDiscovery() *fakeServerGroups {
	return &fakeServerGroups{
		groups: []metav1.APIGroup{{Name: routeAPIGroup}},
	}
}

func TestSelectBestListener_HTTPBeforeHTTPS(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP", "hostname": "maas.example.com"},
		map[string]any{"name": "https", "port": int64(443), "protocol": "HTTPS", "hostname": "maas.example.com"},
	}
	statusByName := map[string]map[string]any{
		"http":  {"name": "http", "attachedRoutes": int64(3)},
		"https": {"name": "https", "attachedRoutes": int64(3)},
	}

	port, protocol, hostname := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(443), port)
	assert.Equal(t, protocolHTTPS, protocol)
	assert.Equal(t, "maas.example.com", hostname)
}

func TestSelectBestListener_TLSProtocol(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "tls", "port": int64(8443), "protocol": "TLS", "hostname": "tls.example.com"},
	}
	statusByName := map[string]map[string]any{
		"tls": {"name": "tls", "attachedRoutes": int64(1)},
	}

	port, protocol, _ := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(8443), port)
	assert.Equal(t, protocolTLS, protocol)
}

func TestSelectBestListener_HTTPOnlyWhenHTTPSNotReady(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP", "hostname": "maas.example.com"},
		map[string]any{"name": "https", "port": int64(443), "protocol": "HTTPS", "hostname": "maas.example.com"},
	}
	statusByName := map[string]map[string]any{
		"http":  {"name": "http", "attachedRoutes": int64(2)},
		"https": {"name": "https", "attachedRoutes": int64(0)},
	}

	port, protocol, _ := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(80), port)
	assert.Equal(t, protocolHTTP, protocol)
}

func TestSelectBestListener_HTTPOnlyListeners(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP", "hostname": "maas.example.com"},
	}
	statusByName := map[string]map[string]any{
		"http": {"name": "http", "attachedRoutes": int64(1)},
	}

	port, protocol, hostname := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(80), port)
	assert.Equal(t, protocolHTTP, protocol)
	assert.Equal(t, "maas.example.com", hostname)
}

func TestSelectBestListener_NoReadyListeners(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "https", "port": int64(443), "protocol": "HTTPS"},
	}
	statusByName := map[string]map[string]any{
		"https": {"name": "https", "attachedRoutes": int64(0)},
	}

	port, protocol, hostname := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(443), port)
	assert.Equal(t, protocolHTTPS, protocol)
	assert.Empty(t, hostname)
}

func TestSelectBestListener_IgnoresTCPListener(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "tcp", "port": int64(9090), "protocol": "TCP", "hostname": "tcp.example.com"},
		map[string]any{"name": "https", "port": int64(443), "protocol": "HTTPS", "hostname": "maas.example.com"},
	}
	statusByName := map[string]map[string]any{
		"tcp":   {"name": "tcp", "attachedRoutes": int64(1)},
		"https": {"name": "https", "attachedRoutes": int64(1)},
	}

	port, protocol, hostname := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(443), port)
	assert.Equal(t, protocolHTTPS, protocol)
	assert.Equal(t, "maas.example.com", hostname)
}

func TestSelectBestListener_TCPOnlyNotUsedAsFallback(t *testing.T) {
	specListeners := []any{
		map[string]any{"name": "tcp", "port": int64(9090), "protocol": "TCP"},
	}
	statusByName := map[string]map[string]any{
		"tcp": {"name": "tcp", "attachedRoutes": int64(1)},
	}

	port, protocol, _ := selectBestListener(specListeners, statusByName)

	assert.Equal(t, int64(443), port)
	assert.Equal(t, protocolHTTPS, protocol)
}
