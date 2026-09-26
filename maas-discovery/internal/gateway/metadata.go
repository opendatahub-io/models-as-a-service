package gateway

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

const (
	protocolHTTPS = "HTTPS"
	protocolTLS   = "TLS"
	protocolHTTP  = "HTTP"
)

// ExtractMetadata extracts connection metadata from a Gateway's unstructured object map.
// On success it returns a fully populated GatewayMetadata. If the gateway is missing
// status, listeners, or an external hostname, it returns an error. Callers should
// degrade gracefully by keeping name/namespace and omitting externalUrl.
func ExtractMetadata(gateway map[string]any, name, namespace string, routeHosts ...map[string]string) (*types.GatewayMetadata, error) {
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

	if strings.HasSuffix(externalHost, ".svc.cluster.local") {
		svcName, _, _ := strings.Cut(externalHost, ".")
		var resolved bool
		for _, rh := range routeHosts {
			if host, ok := rh[svcName]; ok {
				externalHost = host
				resolved = true
				break
			}
		}
		if !resolved {
			return nil, fmt.Errorf("gateway %s/%s has internal service name %s instead of external hostname",
				namespace, name, externalHost)
		}
	}

	scheme := "https"
	if protocol == protocolHTTP {
		scheme = "http"
	}

	externalURL := fmt.Sprintf("%s://%s", scheme, externalHost)
	if (scheme == "https" && port != 443) || (scheme == "http" && port != 80) {
		externalURL = fmt.Sprintf("%s:%d", externalURL, port)
	}

	return &types.GatewayMetadata{
		Name:        name,
		Namespace:   namespace,
		Protocol:    scheme,
		ExternalURL: externalURL,
		Port:        port,
	}, nil
}

// BuildRouteHostMap builds a map from Kubernetes Service name to external hostname
// by inspecting OpenShift Route objects. Returns an empty map if no routes are provided.
func BuildRouteHostMap(routes []unstructured.Unstructured) map[string]string {
	m := make(map[string]string, len(routes))
	for i := range routes {
		spec, ok := routes[i].Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		host, _ := spec["host"].(string)
		if host == "" {
			continue
		}
		to, ok := spec["to"].(map[string]any)
		if !ok {
			continue
		}
		svcName, _ := to["name"].(string)
		if svcName == "" {
			continue
		}
		m[svcName] = host
	}
	return m
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
