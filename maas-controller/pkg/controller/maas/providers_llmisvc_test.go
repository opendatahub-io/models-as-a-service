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
	"context"
	"testing"

	"github.com/go-logr/logr"
	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
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

func newMaaSModelRefForLLMISvc(name, ns, llmisvcName string) *maasv1alpha1.MaaSModelRef {
	return &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: "LLMInferenceService",
				Name: llmisvcName,
			},
		},
	}
}

func TestResolveModelAlias_FromStatusAddresses(t *testing.T) {
	llmisvc := &kservev1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Status: kservev1alpha2.LLMInferenceServiceStatus{
			Addresses: []kservev1alpha2.SourcedAddress{
				{
					Addressable: duckv1.Addressable{URL: mustParseURL("https://gw.example.com/my-model")},
					Models:      []kservev1alpha2.ModelSourcedAddressStatus{{Name: "publishers/model-ns/models/custom-name"}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(llmisvc).Build()
	h := &llmisvcHandler{r: &MaaSModelRefReconciler{Client: c, Scheme: scheme}}
	model := newMaaSModelRefForLLMISvc("ref", "model-ns", "my-model")

	got, err := h.ResolveModelAlias(context.Background(), logr.Discard(), model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "publishers/model-ns/models/custom-name"
	if got != want {
		t.Errorf("ResolveModelAlias() = %q, want %q", got, want)
	}
}

func TestResolveModelAlias_FallbackToSpecModelName(t *testing.T) {
	customName := "meta-llama/llama-3.2-1b-instruct"
	llmisvc := &kservev1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "llama-3-2-1b-instruct", Namespace: "model-server"},
		Spec: kservev1alpha2.LLMInferenceServiceSpec{
			Model: kservev1alpha2.LLMModelSpec{
				URI:  *mustParseURL("oci://quay.io/example/model"),
				Name: &customName,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(llmisvc).Build()
	h := &llmisvcHandler{r: &MaaSModelRefReconciler{Client: c, Scheme: scheme}}
	model := newMaaSModelRefForLLMISvc("ref", "model-server", "llama-3-2-1b-instruct")

	got, err := h.ResolveModelAlias(context.Background(), logr.Discard(), model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "publishers/model-server/models/meta-llama/llama-3.2-1b-instruct"
	if got != want {
		t.Errorf("ResolveModelAlias() = %q, want %q", got, want)
	}
}

func TestResolveModelAlias_FallbackToMetadataName(t *testing.T) {
	llmisvc := &kservev1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: kservev1alpha2.LLMInferenceServiceSpec{
			Model: kservev1alpha2.LLMModelSpec{
				URI: *mustParseURL("oci://quay.io/example/model"),
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(llmisvc).Build()
	h := &llmisvcHandler{r: &MaaSModelRefReconciler{Client: c, Scheme: scheme}}
	model := newMaaSModelRefForLLMISvc("ref", "model-ns", "my-model")

	got, err := h.ResolveModelAlias(context.Background(), logr.Discard(), model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "publishers/model-ns/models/my-model"
	if got != want {
		t.Errorf("ResolveModelAlias() = %q, want %q", got, want)
	}
}

func TestResolveModelAlias_StatusTakesPrecedenceOverSpec(t *testing.T) {
	customName := "spec-model-name"
	llmisvc := &kservev1alpha2.LLMInferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "my-model", Namespace: "model-ns"},
		Spec: kservev1alpha2.LLMInferenceServiceSpec{
			Model: kservev1alpha2.LLMModelSpec{
				URI:  *mustParseURL("oci://quay.io/example/model"),
				Name: &customName,
			},
		},
		Status: kservev1alpha2.LLMInferenceServiceStatus{
			Addresses: []kservev1alpha2.SourcedAddress{
				{
					Addressable: duckv1.Addressable{URL: mustParseURL("https://gw.example.com/my-model")},
					Models:      []kservev1alpha2.ModelSourcedAddressStatus{{Name: "publishers/model-ns/models/status-model-name"}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(llmisvc).Build()
	h := &llmisvcHandler{r: &MaaSModelRefReconciler{Client: c, Scheme: scheme}}
	model := newMaaSModelRefForLLMISvc("ref", "model-ns", "my-model")

	got, err := h.ResolveModelAlias(context.Background(), logr.Discard(), model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "publishers/model-ns/models/status-model-name"
	if got != want {
		t.Errorf("ResolveModelAlias() = %q, want %q (status should take precedence over spec)", got, want)
	}
}
