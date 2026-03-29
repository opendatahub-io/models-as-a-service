// Package externalmodel implements a reconciler that watches MaaSModelRef CRs
// with kind=ExternalModel and creates the Istio resources required to route
// traffic to an external AI model provider:
//
//  1. ExternalName Service   - DNS bridge for HTTPRoute backendRef
//  2. ServiceEntry           - Registers external host in Istio mesh
//  3. DestinationRule        - TLS origination (HTTP -> HTTPS)
//  4. HTTPRoute              - Routes requests and sets Host header
//
// All resources are created in the model's namespace (same as the MaaSModelRef).
// OwnerReferences on each resource ensure Kubernetes garbage collection handles
// cleanup when the MaaSModelRef is deleted.
package externalmodel

import (
	"strings"
)

// ExternalModelSpec holds the configuration for routing to an external model.
// Provider and endpoint are read from the referenced ExternalModel CR (PR #586).
// Port, TLS, path-prefix, and extra-headers are optional annotation overrides on the MaaSModelRef.
type ExternalModelSpec struct {
	// Provider identifies the API format (e.g. "openai", "anthropic", "vllm")
	Provider string
	// Endpoint is the external FQDN (e.g. "api.openai.com")
	Endpoint string
	// ExtraHeaders are additional headers to set (e.g. "anthropic-version=2023-06-01")
	ExtraHeaders map[string]string
	// Port is the external service port (default 443)
	Port int32
	// TLS indicates whether TLS origination is needed (default true)
	TLS bool
	// PathPrefix is the path prefix to match (default "/external/<provider>/")
	PathPrefix string
	// TLSInsecureSkipVerify disables certificate verification (testing only)
	TLSInsecureSkipVerify bool
}

// truncateName ensures base + suffix fits within 63 characters.
func truncateName(base, suffix string) string {
	const maxLen = 63
	max := maxLen - len(suffix)
	if max < 1 {
		max = 1
	}
	if len(base) == 0 {
		base = "model"
	}
	if len(base) > max {
		base = base[:max]
		base = strings.TrimRight(base, "-")
	}
	return base + suffix
}

// ModelRouteName returns the sanitized, length-safe name for the maas-model-* HTTPRoute.
func ModelRouteName(modelName string) string {
	return truncateName("maas-model-"+sanitize(modelName), "")
}

// ModelBackendServiceName returns the sanitized, length-safe name for the backend Service.
func ModelBackendServiceName(modelName string) string {
	return truncateName("maas-model-"+sanitize(modelName), "-backend")
}

// ModelServiceEntryName returns the sanitized, length-safe name for the ServiceEntry.
func ModelServiceEntryName(modelName string) string {
	return truncateName("maas-model-"+sanitize(modelName), "-se")
}

// ModelDestinationRuleName returns the sanitized, length-safe name for the DestinationRule.
func ModelDestinationRuleName(modelName string) string {
	return truncateName("maas-model-"+sanitize(modelName), "-dr")
}

// commonLabels returns labels applied to all managed resources.
func commonLabels(modelName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":       "maas-external-model-reconciler",
		"maas.opendatahub.io/external-model": modelName,
	}
}
