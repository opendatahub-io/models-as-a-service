package gateway

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
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

	// TargetPort and TargetPortName record the Route's spec.port.targetPort
	// (an IntOrString: a Service port number or a Service port name). Route
	// selection uses them to reject Routes whose backend port does not reach
	// a Gateway listener (e.g. a Service exposing only a metrics port).
	// Both zero-value means the Route leaves the backend port unspecified.
	TargetPort     int64
	TargetPortName string
}

// ServicePort is a port exposed by a Service that selects a Gateway.
type ServicePort struct {
	Name string
	// Port is the Service's frontend port.
	Port int64
	// TargetPort is the pod port the frontend port forwards to; Kubernetes
	// defaults it to Port when the Service leaves targetPort unset.
	// TargetPortName is set when the Service's targetPort is a container-port
	// name that cannot be resolved without the pod spec; a numeric Route
	// targetPort cannot be matched against such an entry.
	TargetPort     int64
	TargetPortName string
}

// GatewayServices maps the name of each Service that selects a Gateway's pods
// (i.e. carries the gateway.networking.k8s.io/gateway-name selector label) to
// the ports that Service exposes.
type GatewayServices map[string][]ServicePort

// ExtractMetadata extracts connection metadata from a Gateway's unstructured object map.
// On success it returns a fully populated GatewayMetadata. If the gateway is missing
// status, listeners, or an external hostname, it returns an error. Callers should
// degrade gracefully by keeping name/namespace and omitting externalUrl.
//
// routeHosts maps Service names to admitted external Route endpoint
// candidates (see BuildRouteHostMap). gwServices describes the Services that
// select this Gateway (see SelectGatewayServices); when the Gateway only
// publishes a cluster-local address, the Route fallback looks up endpoints for
// any of those Services whose backend port reaches a Gateway listener, using
// the trusted listener-to-pod-port mapping from listenerPodPorts.
func ExtractMetadata(gateway map[string]any, name, namespace string, routeHosts map[string][]RouteHostInfo, gwServices GatewayServices) (*types.GatewayMetadata, error) {
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
		trustedPodPorts := listenerPodPorts(externalHost, gwServices, gatewayListenerPorts(spec))
		if routeInfo, ok := bestRouteEndpoint(routeHosts, gwServices, trustedPodPorts); ok {
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

// BuildRouteHostMap builds a map from Kubernetes Service name to all admitted
// external Route endpoint candidates targeting that Service, each list sorted
// best-first (HTTPS preferred, then lexicographically by host). Candidates are
// retained rather than collapsed to one per Service because listener-port
// validation happens per Gateway at selection time (see bestRouteEndpoint);
// collapsing early could let an invalid Route (e.g. one targeting a metrics
// port) shadow a valid Route to the same Service.
// Returns an empty map if no routes are provided.
func BuildRouteHostMap(routes []unstructured.Unstructured) map[string][]RouteHostInfo {
	endpointsByService := make(map[string][]RouteHostInfo)
	for i := range routes {
		svcName, info, ok := admittedHostOnlyRouteEndpoint(routes[i].Object)
		if !ok {
			continue
		}
		endpointsByService[svcName] = append(endpointsByService[svcName], info)
	}

	m := make(map[string][]RouteHostInfo, len(endpointsByService))
	for svcName, endpoints := range endpointsByService {
		sort.SliceStable(endpoints, func(left, right int) bool {
			return endpointBefore(endpoints[left], endpoints[right])
		})
		m[svcName] = endpoints
	}
	return m
}

// SelectGatewayServices returns the Services that select the given Gateway's
// pods, i.e. those carrying the gateway.networking.k8s.io/gateway-name
// selector label, mapped to the ports each Service exposes. It mirrors the
// hardened maas-api implementation (PR #1888): Route→Gateway matching is done
// via Service selector labels rather than by parsing the Gateway's internal
// hostname, so Routes targeting any Service that selects the Gateway are
// matched. The exposed ports let Route selection reject backends that do not
// reach a Gateway listener.
func SelectGatewayServices(services []unstructured.Unstructured, gatewayName string) GatewayServices {
	out := make(GatewayServices)
	for i := range services {
		selectorValue, found, err := unstructured.NestedString(services[i].Object, "spec", "selector", gatewayNameLabel)
		if err != nil || !found || selectorValue != gatewayName {
			continue
		}
		name := services[i].GetName()
		out[name] = servicePorts(services[i].Object)
	}
	return out
}

// servicePorts extracts the ports a Service exposes from its unstructured
// object. Ports with a missing or zero port number are skipped. The Service's
// targetPort (an IntOrString) is retained: numeric values resolve to the pod
// port, and its default is the frontend port when unset.
func servicePorts(svc map[string]any) []ServicePort {
	spec, ok := svc["spec"].(map[string]any)
	if !ok {
		return nil
	}
	portsRaw, ok := spec["ports"].([]any)
	if !ok {
		return nil
	}
	ports := make([]ServicePort, 0, len(portsRaw))
	for _, p := range portsRaw {
		portMap, ok := p.(map[string]any)
		if !ok {
			continue
		}
		port := toInt64(portMap["port"])
		if port == 0 {
			continue
		}
		name, _ := portMap["name"].(string)
		sp := ServicePort{Name: name, Port: port, TargetPort: port}
		switch tp := portMap["targetPort"].(type) {
		case string:
			if n, err := strconv.ParseInt(tp, 10, 64); err == nil {
				sp.TargetPort = n
			} else {
				// A container-port name cannot be resolved to a number
				// without the pod spec.
				sp.TargetPort = 0
				sp.TargetPortName = tp
			}
		default:
			if n := toInt64(portMap["targetPort"]); n != 0 {
				sp.TargetPort = n
			}
		}
		ports = append(ports, sp)
	}
	return ports
}

// bestRouteEndpoint picks the best Route endpoint among those targeting any
// Service that selects the Gateway and whose backend port resolves to a pod
// port that serves one of the Gateway's listeners, deterministically: HTTPS
// preferred, then lexicographically by host. All candidates per Service are
// considered so a valid Route is not shadowed by an invalid one targeting the
// same Service. Returns false when no endpoint matches.
func bestRouteEndpoint(routeHosts map[string][]RouteHostInfo, gwServices GatewayServices, trustedPodPorts map[int64]struct{}) (RouteHostInfo, bool) {
	var endpoints []RouteHostInfo
	for svcName, candidates := range routeHosts {
		if _, ok := gwServices[svcName]; !ok {
			continue
		}
		for _, info := range candidates {
			if routeReachesListener(info, gwServices[svcName], trustedPodPorts) {
				endpoints = append(endpoints, info)
			}
		}
	}
	if len(endpoints) == 0 {
		return RouteHostInfo{}, false
	}
	return bestEndpoint(endpoints), true
}

// listenerPodPorts returns the pod ports that serve the Gateway's listeners —
// the trusted listener-to-pod-port mapping Route backends are validated
// against. The Gateway spec only publishes listener frontend ports; the pod
// ports behind them come from Service port translations, and a Service that
// merely selects the Gateway pods can reuse a listener's frontend port number
// for an unrelated pod port (e.g. metrics), so not every translation can be
// trusted:
//
//   - When the Gateway's internal hostname identifies one of the
//     selector-matching Services (its first DNS label is that Service's
//     name — how Gateway API implementations publish their Service), only
//     that Service's translations are trusted: it is the implementation's
//     own Service, so its frontend-to-pod-port mapping is authoritative.
//   - Otherwise, a frontend-to-pod-port mapping is trusted only when every
//     matching Service exposing that frontend agrees on the pod port.
//
// Either way the result fails closed: an empty set rejects all Route
// fallbacks.
func listenerPodPorts(hostname string, gwServices GatewayServices, listenerPorts map[int64]struct{}) map[int64]struct{} {
	trusted := make(map[int64]struct{})
	addTrustedEntry := func(entry ServicePort) {
		if _, isListener := listenerPorts[entry.Port]; isListener && entry.TargetPortName == "" && entry.TargetPort > 0 {
			trusted[entry.TargetPort] = struct{}{}
		}
	}

	if svcName, _, ok := strings.Cut(strings.ToLower(strings.TrimSuffix(hostname, ".")), "."); ok && svcName != "" {
		if entries, found := gwServices[svcName]; found {
			for _, entry := range entries {
				addTrustedEntry(entry)
			}
			return trusted
		}
	}

	// No trusted Service identified: require unanimity. A listener frontend
	// maps to a trusted pod port only when every matching Service exposing
	// that frontend translates it to the same pod port.
	podPortsByFrontend := make(map[int64]map[int64]struct{})
	for _, entries := range gwServices {
		for _, entry := range entries {
			if _, isListener := listenerPorts[entry.Port]; !isListener || entry.TargetPortName != "" || entry.TargetPort <= 0 {
				continue
			}
			if podPortsByFrontend[entry.Port] == nil {
				podPortsByFrontend[entry.Port] = make(map[int64]struct{})
			}
			podPortsByFrontend[entry.Port][entry.TargetPort] = struct{}{}
		}
	}
	for _, podPorts := range podPortsByFrontend {
		if len(podPorts) == 1 {
			for podPort := range podPorts {
				trusted[podPort] = struct{}{}
			}
		}
	}
	return trusted
}

// routeReachesListener reports whether the Route's backend resolves, through
// the given Service, to a pod port that serves one of the Gateway's listeners
// (i.e. a port in the trusted set from listenerPodPorts). A Service selects
// pods, not a listener, so a Service that selects the Gateway pods can expose
// non-listener ports such as metrics; Routes whose backend resolves only to
// such ports must not contribute the Gateway's external URL.
//
// A Route's spec.port.targetPort is an IntOrString naming the pod port: a
// number is the pod port directly, while a name must be a named Service port.
// A Route that omits spec.port is only admitted by the router against a
// single-port Service.
func routeReachesListener(info RouteHostInfo, svcPorts []ServicePort, trustedPodPorts map[int64]struct{}) bool {
	// podPorts: the pod ports the Route's backend resolves to.
	var podPorts []int64
	switch {
	case info.TargetPort != 0:
		// Numeric Route targetPort is the pod port; it must be exposed by the
		// target Service. Entries whose targetPort is a container-port name
		// cannot be resolved and fail closed.
		for _, p := range svcPorts {
			if p.TargetPortName == "" && p.TargetPort == info.TargetPort {
				podPorts = append(podPorts, p.TargetPort)
			}
		}
	case info.TargetPortName != "":
		// Named Route targetPort names a Service port directly.
		for _, p := range svcPorts {
			if p.Name != "" && p.Name == info.TargetPortName && p.TargetPortName == "" && p.TargetPort > 0 {
				podPorts = append(podPorts, p.TargetPort)
			}
		}
	default:
		// No explicit backend port: the router admits the Route against a
		// single-port Service only.
		if len(svcPorts) == 1 && svcPorts[0].TargetPortName == "" && svcPorts[0].TargetPort > 0 {
			podPorts = append(podPorts, svcPorts[0].TargetPort)
		}
	}
	for _, port := range podPorts {
		if _, ok := trustedPodPorts[port]; ok {
			return true
		}
	}
	return false
}

// gatewayListenerPorts returns the set of ports the Gateway's spec listeners
// expose.
func gatewayListenerPorts(spec map[string]any) map[int64]struct{} {
	ports := make(map[int64]struct{})
	listenersRaw, ok := spec["listeners"].([]any)
	if !ok {
		return ports
	}
	for _, l := range listenersRaw {
		listener, ok := l.(map[string]any)
		if !ok {
			continue
		}
		if p := toInt64(listener["port"]); p != 0 {
			ports[p] = struct{}{}
		}
	}
	return ports
}

// bestEndpoint returns the deterministically best endpoint: HTTPS preferred,
// then lexicographically by host.
func bestEndpoint(endpoints []RouteHostInfo) RouteHostInfo {
	sort.SliceStable(endpoints, func(left, right int) bool {
		return endpointBefore(endpoints[left], endpoints[right])
	})
	return endpoints[0]
}

// endpointBefore reports whether left ranks better than right: HTTPS
// preferred, then lexicographically by host.
func endpointBefore(left, right RouteHostInfo) bool {
	if left.Scheme != right.Scheme {
		return left.Scheme == schemeHTTPS
	}
	return left.Host < right.Host
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

	// Retain the Route's backend port (an IntOrString: a Service port number
	// or name) so selection can reject Routes that do not reach a Gateway
	// listener. A targetPort given as a numeric string is treated as a number,
	// matching IntOrString semantics.
	var targetPort int64
	var targetPortName string
	if portSpec, ok := spec["port"].(map[string]any); ok {
		switch tp := portSpec["targetPort"].(type) {
		case string:
			if n, err := strconv.ParseInt(tp, 10, 64); err == nil {
				targetPort = n
			} else {
				targetPortName = tp
			}
		default:
			targetPort = toInt64(portSpec["targetPort"])
		}
	}

	return svcName, RouteHostInfo{
		Host:           host,
		Scheme:         scheme,
		Port:           port,
		TargetPort:     targetPort,
		TargetPortName: targetPortName,
	}, true
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
