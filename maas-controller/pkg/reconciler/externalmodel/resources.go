package externalmodel

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// BuildService creates a Kubernetes ExternalName Service that maps an in-cluster
// DNS name to the external FQDN. This allows HTTPRoute backendRefs to reference
// external hosts via standard k8s Service names.
func BuildService(spec ExternalModelSpec, modelName, namespace string, labels map[string]string) *corev1.Service {
	svcName := ModelBackendServiceName(modelName)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: spec.Endpoint,
			Ports: []corev1.ServicePort{
				{
					Port:       spec.Port,
					TargetPort: intstr.FromInt32(spec.Port),
				},
			},
		},
	}
}

// BuildServiceEntry creates an Istio ServiceEntry that registers the external
// FQDN in the mesh service registry. Required when outboundTrafficPolicy is
// REGISTRY_ONLY.
func BuildServiceEntry(spec ExternalModelSpec, modelName, namespace string, labels map[string]string) *unstructured.Unstructured {
	seName := ModelServiceEntryName(modelName)

	protocol := "HTTPS"
	portName := "https"
	if !spec.TLS {
		protocol = "HTTP"
		portName = "http"
	}

	se := &unstructured.Unstructured{}
	se.SetAPIVersion("networking.istio.io/v1")
	se.SetKind("ServiceEntry")
	se.SetName(seName)
	se.SetNamespace(namespace)
	se.SetLabels(labels)

	se.Object["spec"] = map[string]interface{}{
		"hosts":      []interface{}{spec.Endpoint},
		"location":   "MESH_EXTERNAL",
		"resolution": "DNS",
		"ports": []interface{}{
			map[string]interface{}{
				"number":   int64(spec.Port),
				"name":     portName,
				"protocol": protocol,
			},
		},
	}
	return se
}

// BuildDestinationRule creates an Istio DestinationRule that configures TLS
// origination for the external host. Skipped when TLS is false.
func BuildDestinationRule(spec ExternalModelSpec, modelName, namespace string, labels map[string]string) *unstructured.Unstructured {
	drName := ModelDestinationRuleName(modelName)

	dr := &unstructured.Unstructured{}
	dr.SetAPIVersion("networking.istio.io/v1")
	dr.SetKind("DestinationRule")
	dr.SetName(drName)
	dr.SetNamespace(namespace)
	dr.SetLabels(labels)

	tlsConfig := map[string]interface{}{
		"mode": "SIMPLE",
	}
	if spec.TLSInsecureSkipVerify {
		tlsConfig["insecureSkipVerify"] = true
	}

	dr.Object["spec"] = map[string]interface{}{
		"host": spec.Endpoint,
		"trafficPolicy": map[string]interface{}{
			"tls": tlsConfig,
		},
	}
	return dr
}

// BuildHTTPRoute creates the maas-model-<name> HTTPRoute in the model's namespace.
// This route is used by the MaaS auth and subscription controllers to attach
// AuthPolicy and TokenRateLimitPolicy.
//
// It contains two match rules:
//  1. Path-based match (PathPrefix: /<modelName>) — required for the Kuadrant Wasm plugin
//     which runs before BBR in the Envoy filter chain. Without a path predicate, auth +
//     rate limiting are bypassed.
//  2. Header-based match (X-Gateway-Model-Name: <modelName>) — required for BBR's
//     ClearRouteCache flow. After BBR extracts the model name from the request body,
//     it sets this header and Envoy re-matches to this route.
//
// Both rules route to the backend ExternalName Service in the same namespace and apply
// a URLRewrite filter to strip the path prefix before forwarding to the external provider.
func BuildHTTPRoute(spec ExternalModelSpec, modelName, namespace, gatewayName, gatewayNamespace string, labels map[string]string) *gatewayapiv1.HTTPRoute {
	routeName := ModelRouteName(modelName)
	backendSvcName := ModelBackendServiceName(modelName)

	gwNamespace := gatewayapiv1.Namespace(gatewayNamespace)
	pathType := gatewayapiv1.PathMatchPathPrefix
	pathPrefix := "/" + modelName
	headerType := gatewayapiv1.HeaderMatchExact
	port := gatewayapiv1.PortNumber(spec.Port)
	timeout := gatewayapiv1.Duration("300s")

	backendRefs := []gatewayapiv1.HTTPBackendRef{
		{
			BackendRef: gatewayapiv1.BackendRef{
				BackendObjectReference: gatewayapiv1.BackendObjectReference{
					Name: gatewayapiv1.ObjectName(backendSvcName),
					Port: &port,
				},
			},
		},
	}

	// Build header modifiers (Host + any extra headers)
	headers := []gatewayapiv1.HTTPHeader{
		{
			Name:  "Host",
			Value: spec.Endpoint,
		},
	}
	for k, v := range spec.ExtraHeaders {
		headers = append(headers, gatewayapiv1.HTTPHeader{
			Name:  gatewayapiv1.HTTPHeaderName(k),
			Value: v,
		})
	}

	// Filters shared by both rules: rewrite path prefix and set Host header
	filters := []gatewayapiv1.HTTPRouteFilter{
		{
			Type: gatewayapiv1.HTTPRouteFilterURLRewrite,
			URLRewrite: &gatewayapiv1.HTTPURLRewriteFilter{
				Path: &gatewayapiv1.HTTPPathModifier{
					Type:               gatewayapiv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: strPtr("/"),
				},
			},
		},
		{
			Type: gatewayapiv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayapiv1.HTTPHeaderFilter{
				Set: headers,
			},
		},
	}

	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
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
									Value: modelName,
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

func sanitize(s string) string {
	// Convert to lowercase and replace non-alphanumeric characters with dashes
	// for RFC 1123 DNS label compatibility.
	var result []byte
	for _, c := range []byte(strings.ToLower(s)) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else {
			result = append(result, '-')
		}
	}
	// Trim leading/trailing dashes
	return strings.Trim(string(result), "-")
}

func strPtr(s string) *string {
	return &s
}
