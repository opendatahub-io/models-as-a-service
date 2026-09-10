/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"testing"

	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
)

func strPtr(s string) *string { return &s }

func mustParseURL(raw string) *apis.URL {
	u, err := apis.ParseURL(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func newReadyLLMISvc(name, ns string, addresses []duckv1.Addressable) *kservev1alpha2.LLMInferenceService {
	sourced := make([]kservev1alpha2.SourcedAddress, len(addresses))
	for i, a := range addresses {
		sourced[i] = kservev1alpha2.SourcedAddress{Addressable: a}
	}
	return &kservev1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: kservev1alpha2.LLMInferenceServiceStatus{
			Addresses: sourced,
		},
	}
}

func TestGetEndpointFromLLMISvc_MultipleGateways_CorrectHostname(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://wrong-gateway.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://correct-gateway.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"correct-gateway.example.com"})
	want := "https://correct-gateway.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q", got, want)
	}
}

func TestGetEndpointFromLLMISvc_MultipleGateways_NoMatch(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://gateway-a.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://gateway-b.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"nonexistent.example.com"})
	if got != "" {
		t.Errorf("getEndpointFromLLMISvc() = %q, want empty (should fall through to GetModelEndpoint)", got)
	}
}

func TestGetEndpointFromLLMISvc_NoExpectedHostnames_Legacy(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://first-gateway.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://second-gateway.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, nil)
	want := "https://first-gateway.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (legacy: first HTTPS gateway-external)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_SingleGateway_WithHostnames(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q", got, want)
	}
}

func TestGetEndpointFromLLMISvc_NoExpectedHostnames_FallbackToFirstAddress(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("cluster-local"), URL: mustParseURL("http://test-model.default.svc.cluster.local")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, nil)
	want := "https://test-model.default.svc.cluster.local"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (legacy fallback should upgrade to HTTPS)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_WithHostnames_NoFallbackToWrongGateway(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("cluster-local"), URL: mustParseURL("http://test-model.default.svc.cluster.local")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	if got != "" {
		t.Errorf("getEndpointFromLLMISvc() = %q, want empty (should not fall back when filtering)", got)
	}
}

func TestGetEndpointFromLLMISvc_PrefersHTTPS(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("http://maas.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should prefer HTTPS)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_CaseInsensitiveHostname(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://MaaS.Example.COM/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://MaaS.Example.COM/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (case-insensitive match)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_NilNameAndNilURLSkipped(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: nil, URL: mustParseURL("https://maas.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: nil},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should skip nil-Name and nil-URL addresses)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_EmptyHostnameSkipped(t *testing.T) {
	emptyHostURL := &apis.URL{Path: "/test-model"}
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: emptyHostURL},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should skip address with empty hostname)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_PrefersModelRoutingOverPathBased(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://maas.example.com/v1/chat/completions")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/v1/chat/completions"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (model-routing should be preferred over path-based)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_ModelRouting_HostnameFiltering(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://wrong-gw.example.com/v1/chat/completions")},
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://correct-gw.example.com/v1/chat/completions")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://wrong-gw.example.com/test-model")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://correct-gw.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"correct-gw.example.com"})
	want := "https://correct-gw.example.com/v1/chat/completions"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should select model-routing filtered by hostname)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_FallsBackToPathBased_WhenNoModelRouting(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should fall back to path-based when no model-routing address)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_ModelRouting_PrefersHTTPS(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("http://maas.example.com/v1/chat/completions")},
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://maas.example.com/v1/chat/completions")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	want := "https://maas.example.com/v1/chat/completions"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (should prefer HTTPS model-routing)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_ModelRouting_NoHostnames_Legacy(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://maas.example.com/test-model")},
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://maas.example.com/v1/chat/completions")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, nil)
	want := "https://maas.example.com/v1/chat/completions"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (model-routing preferred in legacy mode too)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_ModelRouting_NoMatch_ReturnsEmpty(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external-model-routing"), URL: mustParseURL("https://other-gw.example.com/v1/chat/completions")},
		{Name: strPtr("gateway-external"), URL: mustParseURL("https://other-gw.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, []string{"maas.example.com"})
	if got != "" {
		t.Errorf("getEndpointFromLLMISvc() = %q, want empty (no matching hostname for any address type)", got)
	}
}

func TestSelectAddress_UpgradesHTTPToHTTPS(t *testing.T) {
	// When only HTTP URLs are available for a named address, selectAddress should
	// upgrade the scheme to HTTPS for the external-facing endpoint.
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("http://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}

	got := h.selectAddress(llmisvc, "gateway-external", nil, false)
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("selectAddress() = %q, want %q (should upgrade HTTP to HTTPS)", got, want)
	}
}

func TestSelectAddress_UpgradesHTTPToHTTPS_WithFiltering(t *testing.T) {
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("gateway-external"), URL: mustParseURL("http://maas.example.com/test-model")},
	})
	h := &llmisvcHandler{}
	hostSet := map[string]struct{}{"maas.example.com": {}}

	got := h.selectAddress(llmisvc, "gateway-external", hostSet, true)
	want := "https://maas.example.com/test-model"
	if got != want {
		t.Errorf("selectAddress() = %q, want %q (should upgrade HTTP to HTTPS with filtering)", got, want)
	}
}

func TestGetEndpointFromLLMISvc_UnfilteredFallback_UpgradesHTTP(t *testing.T) {
	// When the unfiltered legacy fallback returns an HTTP URL from a base-URL address,
	// it should be upgraded to HTTPS.
	llmisvc := newReadyLLMISvc("test-model", "default", []duckv1.Addressable{
		{Name: strPtr("other"), URL: mustParseURL("http://maas.example.com")},
	})
	h := &llmisvcHandler{}

	got := h.getEndpointFromLLMISvc(llmisvc, nil)
	want := "https://maas.example.com"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (unfiltered fallback should upgrade HTTP to HTTPS)", got, want)
	}
}

func TestUpgradeToHTTPS(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"http URL", "http://example.com/path", "https://example.com/path"},
		{"HTTP uppercase", "HTTP://example.com/path", "https://example.com/path"},
		{"Http mixed case", "Http://example.com/path", "https://example.com/path"},
		{"https URL unchanged", "https://example.com/path", "https://example.com/path"},
		{"empty string", "", ""},
		{"non-http scheme", "ftp://example.com", "ftp://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeToHTTPS(tt.input)
			if got != tt.want {
				t.Errorf("upgradeToHTTPS(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
