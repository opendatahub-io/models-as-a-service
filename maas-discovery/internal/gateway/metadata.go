package gateway

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"knative.dev/pkg/network"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

const (
	protocolHTTPS = "HTTPS"
	protocolTLS   = "TLS"
	protocolHTTP  = "HTTP"
	schemeHTTPS   = "https"
	schemeHTTP    = "http"

	// gatewayNameLabel is the selector label Gateway API implementations put
	// on Services that select a Gateway's pods.
	gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
)

// RouteHostInfo stores externally reachable Route endpoint metadata for a
// Gateway Service backend.
type RouteHostInfo struct {
	Host   string
	Scheme string
	Port   int64
}

// ExtractMetadata extracts connection metadata from a Gateway's unstructured object map.
// On success it returns a fully populated GatewayMetadata. If the gateway is missing
// status, listeners, or an external hostname, it returns an error. Callers should
// degrade gracefully by keeping name/namespace and omitting externalUrl.
//
// routeHosts maps Service names to admitted external Route endpoints (see
// BuildRouteHostMap). serviceNames is the set of Services that select this
// Gateway (see GatewayServiceNames); when the Gateway only publishes a
// cluster-local address, the Route fallback looks up endpoints for any of
// those Services.
func ExtractMetadata(gateway map[string]any, name, namespace string, routeHosts map[string]RouteHostInfo, serviceNames map[string]struct{}) (*types.GatewayMetadata, error) {
	spec, ok := gateway["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway spec not found")
	}

	status, ok := gateway["status"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway status not found")
	}

	specListenersRaw, ok := spec["listeners"].([]any)
	if !ok || len(specListenersRaw) == 0 {
		return nil, errors.New("gateway has no listeners in spec")
	}

	statusListenersRaw, ok := status["listeners"].([]any)
	if !ok || len(statusListenersRaw) == 0 {
		return nil, errors.New("gateway has no listeners in status")
	}

	statusListenersByName := make(map[string]map[string]any)
	for _, l := range statusListenersRaw {
		if statusListener, ok := l.(map[string]any); ok {
			if n, ok := statusListener["name"].(string); ok {
				statusListenersByName[n] = statusListener
			}
		}
	}

	port, protocol, hostname, found := selectBestListener(specListenersRaw, statusListenersByName)
	if !found {
		return nil, errors.New("no ready listeners found on gateway")
	}

	externalHost := hostname
	if externalHost == "" {
		if addresses, ok := status["addresses"].([]any); ok && len(addresses) > 0 {
			if addr, ok := addresses[0].(map[string]any); ok {
				if value, ok := addr["value"].(string); ok {
					externalHost = value
				}
			}
		}
	}

	if externalHost == "" {
		return nil, errors.New("could not determine external hostname from gateway")
	}

	if isLocalHostname(externalHost) {
		effectiveScheme, effectivePort := listenerSchemeAndPort(protocol, port)
		if routeInfo, ok := bestRouteEndpoint(routeHosts, serviceNames); ok {
			if routeInfo.Scheme != "" {
				effectiveScheme = routeInfo.Scheme
			}
			if routeInfo.Port != 0 {
				effectivePort = routeInfo.Port
			}
			return buildMetadata(name, namespace, effectiveScheme, routeInfo.Host, effectivePort), nil
		}
		return nil, fmt.Errorf("gateway %s/%s has internal service name %s instead of external hostname",
			namespace, name, externalHost)
	}

	scheme, effectivePort := listenerSchemeAndPort(protocol, port)
	return buildMetadata(name, namespace, scheme, externalHost, effectivePort), nil
}

// buildMetadata assembles GatewayMetadata, bracketing IPv6 literals and
// appending a non-standard port to the external URL.
func buildMetadata(name, namespace, scheme, host string, port int64) *types.GatewayMetadata {
	externalURL := fmt.Sprintf("%s://%s", scheme, bracketIPv6Literal(host))
	if (scheme == schemeHTTPS && port != 443) || (scheme == schemeHTTP && port != 80) {
		externalURL = fmt.Sprintf("%s:%d", externalURL, port)
	}

	return &types.GatewayMetadata{
		Name:        name,
		Namespace:   namespace,
		Protocol:    scheme,
		ExternalURL: externalURL,
		Port:        port,
	}
}

// BuildRouteHostMap builds a map from Kubernetes Service name to admitted external
// Route endpoint metadata. Only admitted, host-only Routes with a non-zero backend
// weight are considered; when multiple Routes target the same Service the winner
// is chosen deterministically (HTTPS preferred, then lexicographically by host).
// Returns an empty map if no routes are provided.
func BuildRouteHostMap(routes []unstructured.Unstructured) map[string]RouteHostInfo {
	endpointsByService := make(map[string][]RouteHostInfo)
	for i := range routes {
		svcName, info, ok := admittedHostOnlyRouteEndpoint(routes[i].Object)
		if !ok {
			continue
		}
		endpointsByService[svcName] = append(endpointsByService[svcName], info)
	}

	m := make(map[string]RouteHostInfo, len(endpointsByService))
	for svcName, endpoints := range endpointsByService {
		m[svcName] = bestEndpoint(endpoints)
	}
	return m
}

// GatewayServiceNames returns the names of the Services that select the given
// Gateway's pods, i.e. those carrying the gateway.networking.k8s.io/gateway-name
// selector label. It mirrors the hardened maas-api implementation (PR #1888):
// Route→Gateway matching is done via Service selector labels rather than by
// parsing the Gateway's internal hostname, so Routes targeting any Service
// that selects the Gateway are matched.
func GatewayServiceNames(services []unstructured.Unstructured, gatewayName string) map[string]struct{} {
	names := make(map[string]struct{})
	for i := range services {
		selectorValue, found, err := unstructured.NestedString(services[i].Object, "spec", "selector", gatewayNameLabel)
		if err != nil || !found || selectorValue != gatewayName {
			continue
		}
		names[services[i].GetName()] = struct{}{}
	}
	return names
}

// bestRouteEndpoint picks the best Route endpoint among those targeting any
// Service in serviceNames, deterministically: HTTPS preferred, then
// lexicographically by host. Returns false when no endpoint matches.
func bestRouteEndpoint(routeHosts map[string]RouteHostInfo, serviceNames map[string]struct{}) (RouteHostInfo, bool) {
	endpoints := make([]RouteHostInfo, 0, len(serviceNames))
	for svcName, info := range routeHosts {
		if _, ok := serviceNames[svcName]; ok {
			endpoints = append(endpoints, info)
		}
	}
	if len(endpoints) == 0 {
		return RouteHostInfo{}, false
	}
	return bestEndpoint(endpoints), true
}

// bestEndpoint returns the deterministically best endpoint: HTTPS preferred,
// then lexicographically by host.
func bestEndpoint(endpoints []RouteHostInfo) RouteHostInfo {
	sort.SliceStable(endpoints, func(left, right int) bool {
		if endpoints[left].Scheme != endpoints[right].Scheme {
			return endpoints[left].Scheme == schemeHTTPS
		}
		return endpoints[left].Host < endpoints[right].Host
	})
	return endpoints[0]
}

// isLocalHostname reports whether a hostname resolves only inside the cluster:
// localhost, loopback/link-local IPs, or names under the cluster domain, .svc,
// .local, .localhost, or .internal suffixes. Private IPs (e.g. 10.x) are
// deliberately not treated as local.
func isLocalHostname(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = host
	}
	if hostname == "localhost" {
		return true
	}

	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}

	clusterDomain := "." + strings.TrimPrefix(network.GetClusterDomainName(), ".")
	localSuffixes := []string{clusterDomain, ".svc", ".local", ".localhost", ".internal"}
	for _, localSuffix := range localSuffixes {
		if strings.HasSuffix(hostname, localSuffix) {
			return true
		}
	}

	return false
}

// bracketIPv6Literal brackets a bare IPv6 literal so it can be embedded in a URL
// (e.g. "fd00::1" becomes "[fd00::1]"). Non-IPv6 hosts are returned unchanged.
func bracketIPv6Literal(host string) string {
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return "[" + host + "]"
	}

	return host
}

// admittedHostOnlyRouteEndpoint extracts the Service name and endpoint metadata
// from an admitted, host-only Route. It mirrors the hardened maas-api
// implementation (PR #1888): path-based Routes are rejected, the host is taken
// from the admitting status.ingress entry rather than spec.host, and disabled
// (weight-0) backends are skipped.
func admittedHostOnlyRouteEndpoint(route map[string]any) (string, RouteHostInfo, bool) {
	spec, ok := route["spec"].(map[string]any)
	if !ok {
		return "", RouteHostInfo{}, false
	}

	to, ok := spec["to"].(map[string]any)
	if !ok {
		return "", RouteHostInfo{}, false
	}
	if kind, _ := to["kind"].(string); kind != "" && kind != "Service" {
		return "", RouteHostInfo{}, false
	}
	svcName, _ := to["name"].(string)
	if svcName == "" {
		return "", RouteHostInfo{}, false
	}

	if weightVal, present := to["weight"]; present && toInt64(weightVal) == 0 {
		return "", RouteHostInfo{}, false
	}

	if path, _ := spec["path"].(string); path != "" {
		return "", RouteHostInfo{}, false
	}

	host := admittedRouteHost(route)
	if host == "" {
		return "", RouteHostInfo{}, false
	}

	scheme := schemeHTTP
	port := int64(80)
	if routeUsesTLS(spec) {
		scheme = schemeHTTPS
		port = 443
	}

	return svcName, RouteHostInfo{Host: host, Scheme: scheme, Port: port}, true
}

// admittedRouteHost returns the host from the Route's admitting
// status.ingress entry. With route sharding or ingress-domain overrides this can
// differ from spec.host; the admitted status host is the authoritative
// externally reachable one.
func admittedRouteHost(route map[string]any) string {
	status, ok := route["status"].(map[string]any)
	if !ok {
		return ""
	}
	ingresses, ok := status["ingress"].([]any)
	if !ok {
		return ""
	}

	for _, ingress := range ingresses {
		ingressMap, ok := ingress.(map[string]any)
		if !ok {
			continue
		}

		host, ok := ingressMap["host"].(string)
		if !ok || host == "" {
			continue
		}

		conditions, ok := ingressMap["conditions"].([]any)
		if !ok {
			continue
		}
		for _, cond := range conditions {
			condMap, ok := cond.(map[string]any)
			if !ok {
				continue
			}
			typeVal, _ := condMap["type"].(string)
			statusVal, _ := condMap["status"].(string)
			if typeVal == "Admitted" && statusVal == "True" {
				return host
			}
		}
	}

	return ""
}

func listenerSchemeAndPort(listenerProtocol string, listenerPort int64) (string, int64) {
	scheme := schemeHTTPS
	if listenerProtocol == protocolHTTP {
		scheme = schemeHTTP
	}
	return scheme, listenerPort
}

func routeUsesTLS(routeSpec map[string]any) bool {
	_, ok := routeSpec["tls"].(map[string]any)
	return ok
}

// selectBestListener picks the best ready listener from a Gateway spec.
// Prefers HTTPS/TLS over HTTP to avoid redirect-induced POST→GET conversion.
// Returns found=false when no listener with attachedRoutes > 0 exists.
func selectBestListener(specListeners []any, statusByName map[string]map[string]any) (int64, string, string, bool) {
	var foundHTTP bool
	var httpPort int64
	var httpHostname string

	for _, l := range specListeners {
		specListener, ok := l.(map[string]any)
		if !ok {
			continue
		}

		listenerName, _ := specListener["name"].(string)
		statusListener, hasStatus := statusByName[listenerName]
		if !hasStatus {
			continue
		}

		attachedRoutes := toInt64(statusListener["attachedRoutes"])
		if attachedRoutes == 0 {
			continue
		}

		listenerProtocol := protocolHTTPS
		if protocolVal, ok := specListener["protocol"].(string); ok {
			listenerProtocol = protocolVal
		}
		listenerPort := int64(443)
		if p := toInt64(specListener["port"]); p != 0 {
			listenerPort = p
		}
		listenerHostname, _ := specListener["hostname"].(string)

		if listenerProtocol == protocolHTTPS || listenerProtocol == protocolTLS {
			return listenerPort, listenerProtocol, listenerHostname, true
		}

		if listenerProtocol == protocolHTTP && !foundHTTP {
			foundHTTP = true
			httpPort = listenerPort
			httpHostname = listenerHostname
		}
	}

	if foundHTTP {
		return httpPort, protocolHTTP, httpHostname, true
	}
	return 0, "", "", false
}

// toInt64 converts an interface value to int64, handling both int64 and float64
// (JSON unmarshal to interface{} uses float64, while the unstructured converter uses int64).
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int32:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}
