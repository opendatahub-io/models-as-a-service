package externalmodel

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// buildService creates a Kubernetes ExternalName Service that maps an in-cluster
// DNS name to the external FQDN. Uses the ExternalModel name directly.
func buildService(endpoint, name, namespace string, port int32, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: endpoint,
			Ports: []corev1.ServicePort{
				{
					Port:       port,
					TargetPort: intstr.FromInt32(port),
				},
			},
		},
	}
}

// buildServiceEntry creates an Istio ServiceEntry that registers the external
// FQDN in the mesh service registry.
func buildServiceEntry(endpoint, name, namespace string, port int32, tls bool, labels map[string]string) *unstructured.Unstructured {
	protocol := "HTTPS"
	portName := "https"
	if !tls {
		protocol = "HTTP"
		portName = "http"
	}

	se := &unstructured.Unstructured{}
	se.SetAPIVersion("networking.istio.io/v1")
	se.SetKind("ServiceEntry")
	se.SetName(name)
	se.SetNamespace(namespace)
	se.SetLabels(labels)

	se.Object["spec"] = map[string]any{
		"hosts":      []any{endpoint},
		"location":   "MESH_EXTERNAL",
		"resolution": "DNS",
		"ports": []any{
			map[string]any{
				"number":   int64(port),
				"name":     portName,
				"protocol": protocol,
			},
		},
	}
	return se
}

// buildDestinationRule creates an Istio DestinationRule that configures TLS
// origination for the external host.
func buildDestinationRule(endpoint, name, namespace string, labels map[string]string) *unstructured.Unstructured {
	dr := &unstructured.Unstructured{}
	dr.SetAPIVersion("networking.istio.io/v1")
	dr.SetKind("DestinationRule")
	dr.SetName(name)
	dr.SetNamespace(namespace)
	dr.SetLabels(labels)

	dr.Object["spec"] = map[string]any{
		"host": endpoint,
		"trafficPolicy": map[string]any{
			"tls": map[string]any{
				"mode": "SIMPLE",
			},
		},
	}
	return dr
}

// buildHTTPRoute creates the HTTPRoute in the model's namespace.
// Path prefix is /<namespace>/<name> for namespace isolation.
// Only a Host header filter is set (required for TLS SNI).
// BBR ext-proc handles path rewriting and provider-specific headers.
func buildHTTPRoute(endpoint, name, targetModel, namespace string, port int32, gatewayName, gatewayNamespace string, labels map[string]string) *gatewayapiv1.HTTPRoute {
	gwNamespace := gatewayapiv1.Namespace(gatewayNamespace)
	pathType := gatewayapiv1.PathMatchPathPrefix
	pathPrefix := "/" + namespace + "/" + name
	headerType := gatewayapiv1.HeaderMatchExact
	gwPort := gatewayapiv1.PortNumber(port)
	timeout := gatewayapiv1.Duration("300s")

	backendRefs := []gatewayapiv1.HTTPBackendRef{
		{
			BackendRef: gatewayapiv1.BackendRef{
				BackendObjectReference: gatewayapiv1.BackendObjectReference{
					Name: gatewayapiv1.ObjectName(name),
					Port: &gwPort,
				},
			},
		},
	}

	// Host header is required for TLS SNI — must be set before TLS handshake,
	// which happens before BBR ext-proc runs.
	filters := []gatewayapiv1.HTTPRouteFilter{
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayapiv1.HTTPHeaderFilter{
				Set: []gatewayapiv1.HTTPHeader{
					{
						Name:  "Host",
						Value: endpoint,
					},
				},
			},
		},
	}

	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{
						Name:      gatewayapiv1.ObjectName(gatewayName),
						Namespace: &gwNamespace,
					},
				},
			},
			Rules: []gatewayapiv1.HTTPRouteRule{
				// Rule 1: Path-based match — Kuadrant Wasm plugin needs this
				{
					Matches: []gatewayapiv1.HTTPRouteMatch{
						{
							Path: &gatewayapiv1.HTTPPathMatch{
								Type:  &pathType,
								Value: &pathPrefix,
							},
						},
					},
					BackendRefs: backendRefs,
					Filters:     filters,
					Timeouts:    &gatewayapiv1.HTTPRouteTimeouts{Request: &timeout},
				},
				// Rule 2: Header-based match — BBR ClearRouteCache sets this header
				{
					Matches: []gatewayapiv1.HTTPRouteMatch{
						{
							Headers: []gatewayapiv1.HTTPHeaderMatch{
								{
									Name:  "X-Gateway-Model-Name",
									Type:  &headerType,
									Value: targetModel,
								},
							},
						},
					},
					BackendRefs: backendRefs,
					Filters:     filters,
					Timeouts:    &gatewayapiv1.HTTPRouteTimeouts{Request: &timeout},
				},
			},
		},
	}
}
