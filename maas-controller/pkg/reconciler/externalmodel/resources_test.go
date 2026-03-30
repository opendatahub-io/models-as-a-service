package externalmodel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitize(t *testing.T) {
	assert.Equal(t, "api-openai-com", sanitize("api.openai.com"))
	assert.Equal(t, "vllm-internal", sanitize("vllm.internal"))
	assert.Equal(t, "simple", sanitize("simple"))
	assert.Equal(t, "api-openai-com", sanitize("API.OpenAI.com")) // uppercase
	assert.Equal(t, "host-8000", sanitize("host:8000"))           // colon
	assert.Equal(t, "my-host", sanitize("my_host"))               // underscore
}

func TestModelNameHelpers(t *testing.T) {
	// Normal names
	assert.Equal(t, "maas-model-my-gpt4", ModelRouteName("my-gpt4"))
	assert.Equal(t, "maas-model-my-gpt4-backend", ModelBackendServiceName("my-gpt4"))
	assert.Equal(t, "maas-model-my-gpt4-se", ModelServiceEntryName("my-gpt4"))
	assert.Equal(t, "maas-model-my-gpt4-dr", ModelDestinationRuleName("my-gpt4"))

	// Names with dots (e.g., model names like "gpt-4o.v2")
	assert.Equal(t, "maas-model-gpt-4o-v2", ModelRouteName("gpt-4o.v2"))
	assert.Equal(t, "maas-model-gpt-4o-v2-backend", ModelBackendServiceName("gpt-4o.v2"))

	// Long names get truncated to 63 chars
	longName := "this-is-a-very-long-model-name-that-exceeds-sixty-three-characters-limit"
	assert.LessOrEqual(t, len(ModelRouteName(longName)), 63)
	assert.LessOrEqual(t, len(ModelBackendServiceName(longName)), 63)
	assert.LessOrEqual(t, len(ModelServiceEntryName(longName)), 63)
	assert.LessOrEqual(t, len(ModelDestinationRuleName(longName)), 63)
}

func TestBuildService(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := commonLabels("my-gpt4")

	svc := BuildService(spec, "my-gpt4", "llm", labels)

	assert.Equal(t, ModelBackendServiceName("my-gpt4"), svc.Name)
	assert.Equal(t, "llm", svc.Namespace)
	assert.Equal(t, "api.openai.com", svc.Spec.ExternalName)
	assert.Equal(t, int32(443), svc.Spec.Ports[0].Port)
	assert.Contains(t, svc.Labels, "maas.opendatahub.io/external-model")
}

func TestBuildServiceEntry(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := commonLabels("my-gpt4")

	se := BuildServiceEntry(spec, "my-gpt4", "llm", labels)

	assert.Equal(t, "ServiceEntry", se.GetKind())
	assert.Equal(t, "networking.istio.io/v1", se.GetAPIVersion())
	assert.Equal(t, ModelServiceEntryName("my-gpt4"), se.GetName())
	assert.Equal(t, "llm", se.GetNamespace())

	hosts := se.Object["spec"].(map[string]interface{})["hosts"].([]interface{})
	assert.Equal(t, "api.openai.com", hosts[0])

	ports := se.Object["spec"].(map[string]interface{})["ports"].([]interface{})
	port := ports[0].(map[string]interface{})
	assert.Equal(t, "https", port["name"])
	assert.Equal(t, "HTTPS", port["protocol"])
}

func TestBuildServiceEntryNoTLS(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "vllm",
		Endpoint: "vllm.internal",
		Port:     8000,
		TLS:      false,
	}
	labels := commonLabels("test-model")

	se := BuildServiceEntry(spec, "test-model", "llm", labels)
	seSpec := se.Object["spec"].(map[string]interface{})
	ports := seSpec["ports"].([]interface{})
	port := ports[0].(map[string]interface{})
	assert.Equal(t, "HTTP", port["protocol"])
	assert.Equal(t, "http", port["name"])
}

func TestBuildDestinationRule(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "openai",
		Endpoint: "api.openai.com",
		Port:     443,
		TLS:      true,
	}
	labels := commonLabels("my-gpt4")

	dr := BuildDestinationRule(spec, "my-gpt4", "llm", labels)

	assert.Equal(t, "DestinationRule", dr.GetKind())
	assert.Equal(t, "networking.istio.io/v1", dr.GetAPIVersion())
	assert.Equal(t, ModelDestinationRuleName("my-gpt4"), dr.GetName())
	assert.Equal(t, "llm", dr.GetNamespace())

	drSpec := dr.Object["spec"].(map[string]interface{})
	assert.Equal(t, "api.openai.com", drSpec["host"])

	// Default: no insecureSkipVerify key
	tlsCfg := drSpec["trafficPolicy"].(map[string]interface{})["tls"].(map[string]interface{})
	assert.Equal(t, "SIMPLE", tlsCfg["mode"])
	_, hasInsecure := tlsCfg["insecureSkipVerify"]
	assert.False(t, hasInsecure, "insecureSkipVerify should not be set by default")
}

func TestBuildDestinationRuleInsecureSkipVerify(t *testing.T) {
	spec := ExternalModelSpec{
		Provider:              "openai",
		Endpoint:              "3.150.113.9",
		Port:                  443,
		TLS:                   true,
		TLSInsecureSkipVerify: true,
	}
	labels := commonLabels("simulator-model")

	dr := BuildDestinationRule(spec, "simulator-model", "llm", labels)

	drSpec := dr.Object["spec"].(map[string]interface{})
	assert.Equal(t, "3.150.113.9", drSpec["host"])

	tlsCfg := drSpec["trafficPolicy"].(map[string]interface{})["tls"].(map[string]interface{})
	assert.Equal(t, "SIMPLE", tlsCfg["mode"])
	assert.Equal(t, true, tlsCfg["insecureSkipVerify"], "insecureSkipVerify must be true when opted in")
}

func TestBuildHTTPRoute(t *testing.T) {
	spec := ExternalModelSpec{
		Provider:     "openai",
		Endpoint:     "api.openai.com",
		Port:         443,
		TLS:          true,
		ExtraHeaders: map[string]string{},
	}
	labels := commonLabels("my-gpt4")

	hr := BuildHTTPRoute(spec, "my-gpt4", "llm", "maas-default-gateway", "openshift-ingress", labels)

	assert.Equal(t, ModelRouteName("my-gpt4"), hr.Name)
	assert.Equal(t, "llm", hr.Namespace)
	assert.Len(t, hr.Spec.ParentRefs, 1)
	assert.Equal(t, "maas-default-gateway", string(hr.Spec.ParentRefs[0].Name))

	// Must have 2 rules: path-based and header-based
	assert.Len(t, hr.Spec.Rules, 2, "must have path-based and header-based rules")

	// Rule 1: path-based match
	rule1 := hr.Spec.Rules[0]
	assert.Len(t, rule1.Matches, 1)
	assert.NotNil(t, rule1.Matches[0].Path)
	assert.Equal(t, "/my-gpt4", *rule1.Matches[0].Path.Value)
	assert.Equal(t, ModelBackendServiceName("my-gpt4"), string(rule1.BackendRefs[0].Name))

	// Rule 2: header-based match
	rule2 := hr.Spec.Rules[1]
	assert.Len(t, rule2.Matches, 1)
	assert.Len(t, rule2.Matches[0].Headers, 1)
	assert.Equal(t, "X-Gateway-Model-Name", string(rule2.Matches[0].Headers[0].Name))
	assert.Equal(t, "my-gpt4", rule2.Matches[0].Headers[0].Value)
	assert.Equal(t, ModelBackendServiceName("my-gpt4"), string(rule2.BackendRefs[0].Name))

	// Both rules should have URLRewrite filter
	for i, rule := range hr.Spec.Rules {
		foundRewrite := false
		for _, f := range rule.Filters {
			if f.URLRewrite != nil {
				foundRewrite = true
				assert.Equal(t, "/", *f.URLRewrite.Path.ReplacePrefixMatch,
					"rule %d: URLRewrite should strip prefix to /", i)
			}
		}
		assert.True(t, foundRewrite, "rule %d: must have URLRewrite filter", i)
	}
}

func TestBuildHTTPRouteWithExtraHeaders(t *testing.T) {
	spec := ExternalModelSpec{
		Provider: "anthropic",
		Endpoint: "api.anthropic.com",
		Port:     443,
		TLS:      true,
		ExtraHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
		},
	}
	labels := commonLabels("my-claude")

	hr := BuildHTTPRoute(spec, "my-claude", "llm", "maas-default-gateway", "openshift-ingress", labels)

	// Check both rules have the extra header
	for _, rule := range hr.Spec.Rules {
		for _, f := range rule.Filters {
			if f.RequestHeaderModifier != nil {
				foundHost := false
				foundExtra := false
				for _, h := range f.RequestHeaderModifier.Set {
					if string(h.Name) == "Host" {
						foundHost = true
						assert.Equal(t, "api.anthropic.com", h.Value)
					}
					if string(h.Name) == "anthropic-version" {
						foundExtra = true
						assert.Equal(t, "2023-06-01", h.Value)
					}
				}
				assert.True(t, foundHost, "must set Host header")
				assert.True(t, foundExtra, "must set anthropic-version header")
			}
		}
	}
}
