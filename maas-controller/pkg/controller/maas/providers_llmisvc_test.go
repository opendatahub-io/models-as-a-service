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

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
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

func newReadyLLMISvc(name, ns string, addresses []duckv1.Addressable) *kservev1alpha1.LLMInferenceService {
	return &kservev1alpha1.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: kservev1alpha1.LLMInferenceServiceStatus{
			AddressStatus: duckv1.AddressStatus{
				Addresses: addresses,
			},
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
	want := "http://test-model.default.svc.cluster.local"
	if got != want {
		t.Errorf("getEndpointFromLLMISvc() = %q, want %q (legacy fallback to first address)", got, want)
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
