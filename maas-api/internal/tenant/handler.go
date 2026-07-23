package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	gatewayMetadata, err := h.extractGatewayMetadata(gateway.Object)
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
func (h *Handler) extractGatewayMetadata(gateway map[string]any) (*GatewayMetadata, error) {
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

	// Find first ready listener (has attached routes in status)
	var port int64 = 443   // default
	var protocol = "HTTPS" // default
	var hostname string

	for _, l := range specListenersRaw {
		specListener, ok := l.(map[string]any)
		if !ok {
			continue
		}

		// Get listener name to check status
		listenerName, _ := specListener["name"].(string)
		statusListener, hasStatus := statusListenersByName[listenerName]

		// Check if listener is ready (has attached routes)
		if hasStatus {
			attachedRoutes, _ := statusListener["attachedRoutes"].(int64)
			if attachedRoutes == 0 {
				continue
			}
		} else {
			// No status for this listener, skip
			continue
		}

		// Extract port from spec
		if portVal, ok := specListener["port"].(int64); ok {
			port = portVal
		}

		// Extract protocol from spec
		if protocolVal, ok := specListener["protocol"].(string); ok {
			protocol = protocolVal
		}

		// Extract hostname from spec
		if hostnameVal, ok := specListener["hostname"].(string); ok {
			hostname = hostnameVal
		}

		// Use first ready listener
		break
	}

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

	// Reject internal service names - better to return error than misleading internal hostname
	if strings.HasSuffix(externalHost, ".svc.cluster.local") {
		h.log.Warn("Gateway has no external hostname configured",
			"gateway", h.gatewayName,
			"namespace", h.gatewayNamespace,
			"internal_address", externalHost,
			"recommendation", "Configure Gateway with hostname in spec.listeners and LoadBalancer service type")
		return nil, fmt.Errorf("gateway %s/%s is not configured with an external hostname (found internal service name: %s). "+
			"Please configure the Gateway with spec.listeners[].hostname and use LoadBalancer service type",
			h.gatewayNamespace, h.gatewayName, externalHost)
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
