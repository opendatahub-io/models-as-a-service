package tenant

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"knative.dev/pkg/network"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
)

const (
	protocolHTTPS = "HTTPS"
	protocolTLS   = "TLS"
	protocolHTTP  = "HTTP"

	gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
	routeAPIGroup    = "route.openshift.io"
	routeKindService = "Service"
	routeAdmitted    = "Admitted"
	conditionTrue    = "True"
)

// Handler handles /v1/tenant endpoint requests.
type Handler struct {
	log              *logger.Logger
	dynamicClient    dynamic.Interface
	routeDiscovery   discovery.ServerGroupsInterfaceWithContext
	tenantName       string
	gatewayName      string
	gatewayNamespace string
}

// NewHandler creates a new tenant handler.
func NewHandler(
	log *logger.Logger,
	dynamicClient dynamic.Interface,
	routeDiscovery discovery.ServerGroupsInterfaceWithContext,
	tenantName, gatewayName, gatewayNamespace string,
) *Handler {
	return &Handler{
		log:              log,
		dynamicClient:    dynamicClient,
		routeDiscovery:   routeDiscovery,
		tenantName:       tenantName,
		gatewayName:      gatewayName,
		gatewayNamespace: gatewayNamespace,
	}
}

// TenantsResponse represents the response for GET /v1/tenants.
// For 3.5, returns a single-element array containing this instance's tenant.
type TenantsResponse struct {
	Tenants []TenantInfo `json:"tenants"`
}

// TenantInfo contains tenant identification and gateway metadata.
type TenantInfo struct {
	Name    string          `json:"name"`
	Gateway GatewayMetadata `json:"gateway"`
}

// GatewayMetadata contains gateway connection details.
type GatewayMetadata struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Protocol    string `json:"protocol"`
	ExternalURL string `json:"externalUrl"`
	Port        int64  `json:"port"`
}

// GetTenantInfo returns tenant name and gateway connection metadata.
// GET /v1/tenants.
func (h *Handler) GetTenantInfo(c *gin.Context) {
	requestHandler := *h
	if requestLogger := middleware.GetLogger(c); requestLogger != nil {
		requestHandler.log = requestLogger
	} else {
		requestHandler.log = h.log.WithContext(c.Request.Context())
	}
	h = &requestHandler

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Fetch Gateway resource
	gatewayGVR := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}

	gateway, err := h.dynamicClient.Resource(gatewayGVR).Namespace(h.gatewayNamespace).Get(ctx, h.gatewayName, metav1.GetOptions{})
	if err != nil {
		h.log.Error("Failed to fetch Gateway",
			"gatewayName", h.gatewayName,
			"gatewayNamespace", h.gatewayNamespace,
			"error", err)

		// Distinguish NotFound from other Kubernetes API errors
		if apierrors.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "Gateway not found",
				"details": fmt.Sprintf("Gateway %s not found in namespace %s", h.gatewayName, h.gatewayNamespace),
			})
			return
		}

		// RBAC, timeout, or apiserver issues
		statusCode := http.StatusInternalServerError
		if apierrors.IsForbidden(err) {
			statusCode = http.StatusForbidden
		} else if apierrors.IsTimeout(err) {
			statusCode = http.StatusGatewayTimeout
		}

		c.JSON(statusCode, gin.H{
			"error":   "Failed to fetch Gateway",
			"details": "Unable to query Gateway resource",
		})
		return
	}

	// Extract gateway metadata
	gatewayMetadata, err := h.extractGatewayMetadata(ctx, gateway.Object)
	if err != nil {
		h.log.Error("Failed to extract gateway metadata", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to resolve gateway",
			"details": "Unable to determine external hostname from Gateway status",
		})
		return
	}

	response := TenantsResponse{
		Tenants: []TenantInfo{
			{
				Name:    h.tenantName,
				Gateway: *gatewayMetadata,
			},
		},
	}

	c.JSON(http.StatusOK, response)
}

// extractGatewayMetadata extracts connection metadata from Gateway spec and status.
// Hostname, port, and protocol come from spec.listeners.
// Readiness (attachedRoutes) comes from status.listeners.
func (h *Handler) extractGatewayMetadata(ctx context.Context, gateway map[string]any) (*GatewayMetadata, error) {
	// Extract spec
	spec, ok := gateway["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway spec not found")
	}

	// Extract status
	status, ok := gateway["status"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway status not found")
	}

	// Extract listeners from spec (contains hostname, port, protocol)
	specListenersRaw, ok := spec["listeners"].([]any)
	if !ok || len(specListenersRaw) == 0 {
		return nil, errors.New("gateway has no listeners in spec")
	}

	// Extract listeners from status (contains attachedRoutes for readiness)
	statusListenersRaw, ok := status["listeners"].([]any)
	if !ok || len(statusListenersRaw) == 0 {
		return nil, errors.New("gateway has no listeners in status")
	}

	// Build map of status listeners by name for quick lookup
	statusListenersByName := make(map[string]map[string]any)
	for _, l := range statusListenersRaw {
		if statusListener, ok := l.(map[string]any); ok {
			if name, ok := statusListener["name"].(string); ok {
				statusListenersByName[name] = statusListener
			}
		}
	}

	port, protocol, hostname := selectBestListener(specListenersRaw, statusListenersByName)

	// Get external hostname from Gateway status
	externalHost := hostname

	// Fallback: try status.addresses (set by Gateway controller with external hostname)
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

	// A cluster-local Gateway address can still be exposed by an OpenShift Route.
	if isLocalHostname(externalHost) {
		routeMetadata, found, err := h.openShiftRouteFallback(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to discover an OpenShift Route for gateway %s/%s: %w",
				h.gatewayNamespace, h.gatewayName, err)
		}
		if found {
			h.log.Info("Using OpenShift Route for cluster-local Gateway",
				"gateway", h.gatewayName,
				"namespace", h.gatewayNamespace,
				"externalUrl", routeMetadata.ExternalURL)
			return routeMetadata, nil
		}

		h.log.Warn("Gateway has no external hostname or compatible OpenShift Route configured",
			"gateway", h.gatewayName,
			"namespace", h.gatewayNamespace,
			"internal_address", externalHost,
			"recommendation", "Configure Gateway with hostname in spec.listeners and LoadBalancer service type, or add an admitted host-only OpenShift Route to a Gateway Service")
		return nil, fmt.Errorf("gateway %s/%s is not configured with an external hostname or compatible OpenShift Route (found internal service name: %s). "+
			"Please configure the Gateway with spec.listeners[].hostname and use LoadBalancer service type, or add an admitted host-only OpenShift Route to a Gateway Service",
			h.gatewayNamespace, h.gatewayName, externalHost)
	}

	// Build external URL
	scheme := "https"
	if protocol == protocolHTTP {
		scheme = "http"
	}

	externalURL := fmt.Sprintf("%s://%s", scheme, externalHost)
	// Include port if non-standard
	if (scheme == "https" && port != 443) || (scheme == "http" && port != 80) {
		externalURL = fmt.Sprintf("%s:%d", externalURL, port)
	}

	return &GatewayMetadata{
		Name:        h.gatewayName,
		Namespace:   h.gatewayNamespace,
		Protocol:    scheme,
		ExternalURL: externalURL,
		Port:        port,
	}, nil
}

type routeEndpoint struct {
	ExternalURL string
	Protocol    string
	Port        int64
}

func (h *Handler) routeAPIAvailable(ctx context.Context) (bool, error) {
	groupList, err := h.routeDiscovery.ServerGroupsWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("discovering API groups: %w", err)
	}

	for _, group := range groupList.Groups {
		if group.Name == routeAPIGroup {
			return true, nil
		}
	}

	return false, nil
}

func isLocalHostname(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	if host, _, err := net.SplitHostPort(hostname); err == nil {
		hostname = host
	}
	if hostname == "localhost" {
		return true
	}

	if ip := net.ParseIP(hostname); ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
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

// openShiftRouteFallback mirrors Kserve's Route discovery for Gateways that only
// expose cluster-local addresses. It finds admitted, host-only Routes targeting
// a Service whose selector identifies the Gateway.
func (h *Handler) openShiftRouteFallback(ctx context.Context) (*GatewayMetadata, bool, error) {
	if h.dynamicClient == nil || h.routeDiscovery == nil {
		return nil, false, nil
	}

	routeAPIAvailable, err := h.routeAPIAvailable(ctx)
	if err != nil {
		return nil, false, err
	}
	if !routeAPIAvailable {
		h.log.Info("OpenShift Route API unavailable; skipping Route fallback")
		return nil, false, nil
	}

	serviceGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	services, err := h.dynamicClient.Resource(serviceGVR).Namespace(h.gatewayNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("failed to list Services in namespace %s: %w", h.gatewayNamespace, err)
	}

	serviceNames := gatewayServiceNames(services, h.gatewayName)
	if len(serviceNames) == 0 {
		return nil, false, nil
	}

	routeGVR := schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"}
	routes, err := h.dynamicClient.Resource(routeGVR).Namespace(h.gatewayNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			h.log.Info("OpenShift Route API unavailable; skipping Route fallback")
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to list Routes in namespace %s: %w", h.gatewayNamespace, err)
	}

	endpoints := make([]routeEndpoint, 0)
	for _, route := range routes.Items {
		if endpoint, ok := admittedHostOnlyRouteEndpoint(route, serviceNames); ok {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return nil, false, nil
	}

	sort.SliceStable(endpoints, func(left, right int) bool {
		if endpoints[left].Protocol != endpoints[right].Protocol {
			return endpoints[left].Protocol == "https"
		}
		return endpoints[left].ExternalURL < endpoints[right].ExternalURL
	})
	endpoint := endpoints[0]

	return &GatewayMetadata{
		Name:        h.gatewayName,
		Namespace:   h.gatewayNamespace,
		Protocol:    endpoint.Protocol,
		ExternalURL: endpoint.ExternalURL,
		Port:        endpoint.Port,
	}, true, nil
}

func gatewayServiceNames(services *unstructured.UnstructuredList, gatewayName string) map[string]struct{} {
	serviceNames := make(map[string]struct{})
	for _, service := range services.Items {
		selectorValue, found, err := unstructured.NestedString(service.Object, "spec", "selector", gatewayNameLabel)
		if err != nil || !found || selectorValue != gatewayName {
			continue
		}
		serviceNames[service.GetName()] = struct{}{}
	}
	return serviceNames
}

func admittedHostOnlyRouteEndpoint(route unstructured.Unstructured, serviceNames map[string]struct{}) (routeEndpoint, bool) {
	targetKind, _, _ := unstructured.NestedString(route.Object, "spec", "to", "kind")
	if targetKind != "" && targetKind != routeKindService {
		return routeEndpoint{}, false
	}

	targetName, _, _ := unstructured.NestedString(route.Object, "spec", "to", "name")
	if _, targetsGateway := serviceNames[targetName]; !targetsGateway {
		return routeEndpoint{}, false
	}

	routePath, found, err := unstructured.NestedString(route.Object, "spec", "path")
	if err != nil || found && routePath != "" {
		return routeEndpoint{}, false
	}

	host := admittedRouteHost(route)
	if host == "" {
		return routeEndpoint{}, false
	}

	protocol := "http"
	port := int64(80)
	if _, hasTLS, _ := unstructured.NestedMap(route.Object, "spec", "tls"); hasTLS {
		protocol = "https"
		port = 443
	}

	return routeEndpoint{
		ExternalURL: fmt.Sprintf("%s://%s", protocol, host),
		Protocol:    protocol,
		Port:        port,
	}, true
}

func admittedRouteHost(route unstructured.Unstructured) string {
	ingresses, found, err := unstructured.NestedSlice(route.Object, "status", "ingress")
	if err != nil || !found {
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

		conditions, found, _ := unstructured.NestedSlice(ingressMap, "conditions")
		if !found {
			continue
		}
		for _, condition := range conditions {
			conditionMap, ok := condition.(map[string]any)
			if !ok {
				continue
			}

			conditionType, _ := conditionMap["type"].(string)
			conditionStatus, _ := conditionMap["status"].(string)
			if conditionType == routeAdmitted && conditionStatus == conditionTrue {
				return host
			}
		}
	}

	return ""
}

// selectBestListener picks the best ready listener from a Gateway spec.
// Prefers HTTPS/TLS over HTTP to avoid redirect-induced POST→GET conversion.
func selectBestListener(specListeners []any, statusByName map[string]map[string]any) (int64, string, string) {
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
		attachedRoutes, _ := statusListener["attachedRoutes"].(int64)
		if attachedRoutes == 0 {
			continue
		}

		listenerProtocol := protocolHTTPS
		if protocolVal, ok := specListener["protocol"].(string); ok {
			listenerProtocol = protocolVal
		}
		listenerPort := int64(443)
		if portVal, ok := specListener["port"].(int64); ok {
			listenerPort = portVal
		}
		listenerHostname, _ := specListener["hostname"].(string)

		if listenerProtocol == protocolHTTPS || listenerProtocol == protocolTLS {
			return listenerPort, listenerProtocol, listenerHostname
		}

		if listenerProtocol == protocolHTTP && !foundHTTP {
			foundHTTP = true
			httpPort = listenerPort
			httpHostname = listenerHostname
		}
	}

	if foundHTTP {
		return httpPort, protocolHTTP, httpHostname
	}
	// No ready listeners found; return safe defaults.
	return 443, protocolHTTPS, ""
}
