package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// Handler handles /v1/tenant endpoint requests.
type Handler struct {
	log              *logger.Logger
	dynamicClient    dynamic.Interface
	tenantName       string
	gatewayName      string
	gatewayNamespace string
}

// NewHandler creates a new tenant handler.
func NewHandler(log *logger.Logger, dynamicClient dynamic.Interface, tenantName, gatewayName, gatewayNamespace string) *Handler {
	return &Handler{
		log:              log,
		dynamicClient:    dynamicClient,
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
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "Gateway not found",
			"details": fmt.Sprintf("Gateway %s not found in namespace %s", h.gatewayName, h.gatewayNamespace),
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

// extractGatewayMetadata extracts connection metadata from Gateway status.
// It attempts to find the external hostname from OpenShift Routes first,
// falling back to Gateway status if no Route is found.
func (h *Handler) extractGatewayMetadata(ctx context.Context, gateway map[string]any) (*GatewayMetadata, error) {
	// Extract status
	status, ok := gateway["status"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway status not found")
	}

	// Extract listeners from status to get port and protocol
	listenersRaw, ok := status["listeners"].([]any)
	if !ok || len(listenersRaw) == 0 {
		return nil, errors.New("gateway has no listeners in status")
	}

	// Find first ready listener (has attached routes)
	var port int64 = 443 // default
	var protocol = "HTTPS" // default
	var hostname string

	for _, l := range listenersRaw {
		listener, ok := l.(map[string]any)
		if !ok {
			continue
		}

		// Get attached routes count to determine if listener is ready
		attachedRoutes, _ := listener["attachedRoutes"].(int64)
		if attachedRoutes == 0 {
			continue
		}

		// Extract port
		if portVal, ok := listener["port"].(int64); ok {
			port = portVal
		}

		// Extract protocol
		if protocolVal, ok := listener["protocol"].(string); ok {
			protocol = protocolVal
		}

		// Extract hostname for fallback
		if hostnameVal, ok := listener["hostname"].(string); ok {
			hostname = hostnameVal
		}

		// Use first ready listener
		break
	}

	// Try to find external hostname from OpenShift Route (preferred for external access)
	externalHost := h.findRouteHostname(ctx)

	// Fallback: try hostname from listener
	if externalHost == "" {
		externalHost = hostname
	}

	// Fallback: try status.addresses
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

	// Build external URL
	scheme := "https"
	if protocol == "HTTP" {
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

// findRouteHostname attempts to find the external hostname from OpenShift Routes
// that reference this Gateway. Returns empty string if no Route is found.
func (h *Handler) findRouteHostname(ctx context.Context) string {
	// Skip if dynamic client is not available (e.g., in unit tests)
	if h.dynamicClient == nil {
		return ""
	}

	// OpenShift Route GVR
	routeGVR := schema.GroupVersionResource{
		Group:    "route.openshift.io",
		Version:  "v1",
		Resource: "routes",
	}

	// List Routes in the Gateway's namespace
	routes, err := h.dynamicClient.Resource(routeGVR).Namespace(h.gatewayNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Routes may not exist (non-OpenShift cluster) or no RBAC - not an error
		h.log.Debug("Could not list Routes (may not be OpenShift)", "error", err)
		return ""
	}

	// Find Route that references this Gateway
	gatewayServiceName := fmt.Sprintf("%s-openshift-default", h.gatewayName)

	for _, item := range routes.Items {
		spec, ok := item.Object["spec"].(map[string]any)
		if !ok {
			continue
		}

		to, ok := spec["to"].(map[string]any)
		if !ok {
			continue
		}

		// Check if Route targets the Gateway Service
		serviceName, ok := to["name"].(string)
		if !ok || serviceName != gatewayServiceName {
			continue
		}

		// Extract hostname from Route spec
		if host, ok := spec["host"].(string); ok && host != "" {
			h.log.Debug("Found external hostname from Route",
				"route", item.GetName(),
				"hostname", host)
			return host
		}
	}

	return ""
}
