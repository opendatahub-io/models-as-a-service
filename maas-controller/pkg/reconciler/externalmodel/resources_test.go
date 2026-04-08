package externalmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestBuildService(t *testing.T) {
	svc := buildService("api.openai.com", "gpt-4o", "llm", 443, commonLabels("gpt-4o"))

	assert.Equal(t, "gpt-4o", svc.Name)
	assert.Equal(t, "llm", svc.Namespace)
	assert.Equal(t, "api.openai.com", svc.Spec.ExternalName)
	assert.Equal(t, int32(443), svc.Spec.Ports[0].Port)
}

func TestBuildServiceEntry(t *testing.T) {
	se := buildServiceEntry("api.openai.com", "gpt-4o", "llm", 443, true, commonLabels("gpt-4o"))

	assert.Equal(t, "ServiceEntry", se.GetKind())
	assert.Equal(t, "gpt-4o", se.GetName())
	assert.Equal(t, "llm", se.GetNamespace())

	seSpec, ok := se.Object["spec"].(map[string]any)
	require.True(t, ok)
	hosts, ok := seSpec["hosts"].([]any)
	require.True(t, ok)
	assert.Equal(t, "api.openai.com", hosts[0])

	ports, ok := seSpec["ports"].([]any)
	require.True(t, ok)
	port, ok := ports[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https", port["name"])
	assert.Equal(t, "HTTPS", port["protocol"])
}

func TestBuildServiceEntryNoTLS(t *testing.T) {
	se := buildServiceEntry("vllm.internal", "my-vllm", "llm", 8000, false, commonLabels("my-vllm"))

	seSpec, ok := se.Object["spec"].(map[string]any)
	require.True(t, ok)
	ports, ok := seSpec["ports"].([]any)
	require.True(t, ok)
	port, ok := ports[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "HTTP", port["protocol"])
	assert.Equal(t, "http", port["name"])
}

func TestBuildDestinationRule(t *testing.T) {
	dr := buildDestinationRule("api.openai.com", "gpt-4o", "llm", commonLabels("gpt-4o"))

	assert.Equal(t, "DestinationRule", dr.GetKind())
	assert.Equal(t, "gpt-4o", dr.GetName())
	assert.Equal(t, "llm", dr.GetNamespace())

	drSpec, ok := dr.Object["spec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api.openai.com", drSpec["host"])
}

func TestBuildHTTPRoute(t *testing.T) {
	hr := buildHTTPRoute("api.openai.com", "gpt-4o", "llm", 443, "maas-default-gateway", "openshift-ingress", commonLabels("gpt-4o"))

	assert.Equal(t, "gpt-4o", hr.Name)
	assert.Equal(t, "llm", hr.Namespace)
	assert.Len(t, hr.Spec.ParentRefs, 1)
	assert.Equal(t, "maas-default-gateway", string(hr.Spec.ParentRefs[0].Name))

	// Must have 2 rules: path-based and header-based
	assert.Len(t, hr.Spec.Rules, 2)

	// Rule 1: path-based match with namespace prefix
	rule1 := hr.Spec.Rules[0]
	assert.Equal(t, "/llm/gpt-4o", *rule1.Matches[0].Path.Value)
	assert.Equal(t, "gpt-4o", string(rule1.BackendRefs[0].Name))

	// Rule 2: header-based match
	rule2 := hr.Spec.Rules[1]
	assert.Equal(t, "X-Gateway-Model-Name", string(rule2.Matches[0].Headers[0].Name))
	assert.Equal(t, "gpt-4o", rule2.Matches[0].Headers[0].Value)

	// Only Host header filter (required for TLS SNI), no URLRewrite
	for i, rule := range hr.Spec.Rules {
		assert.Len(t, rule.Filters, 1, "rule %d: must have exactly 1 filter (Host header)", i)
		assert.Equal(t, gatewayapiv1.HTTPRouteFilterRequestHeaderModifier, rule.Filters[0].Type)
		assert.Equal(t, "Host", string(rule.Filters[0].RequestHeaderModifier.Set[0].Name))
		assert.Equal(t, "api.openai.com", rule.Filters[0].RequestHeaderModifier.Set[0].Value)
	}
}
