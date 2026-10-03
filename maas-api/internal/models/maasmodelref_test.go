package models_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
)

func newMaaSModelRefUnstructured(endpoint string, hostnames []string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "maas.opendatahub.io",
		Version: "v1alpha1",
		Kind:    "MaaSModelRef",
	})
	u.SetName("test-model")
	u.SetNamespace("default")
	u.SetCreationTimestamp(metav1.NewTime(time.Unix(1700000000, 0)))
	_ = unstructured.SetNestedField(u.Object, endpoint, "status", "endpoint")
	_ = unstructured.SetNestedField(u.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedField(u.Object, "llmisvc", "spec", "modelRef", "kind")
	if len(hostnames) > 0 {
		hostnameInterfaces := make([]any, len(hostnames))
		for i, h := range hostnames {
			hostnameInterfaces[i] = h
		}
		_ = unstructured.SetNestedSlice(u.Object, hostnameInterfaces, "status", "httpRouteHostnames")
	}
	return u
}

func TestMaasModelRefToModel_HostnamesPreferred(t *testing.T) {
	u := newMaaSModelRefUnstructured("http://internal.svc.cluster.local/model-a", []string{"maas.example.com"})
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_NormalizesToHTTPSBaseURL(t *testing.T) {
	u := newMaaSModelRefUnstructured("http://maas.example.com/v1/chat/completions", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_ClearsQueryAndFragment(t *testing.T) {
	u := newMaaSModelRefUnstructured("http://maas.example.com/model?x=1#frag", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_UppercaseHTTP(t *testing.T) {
	u := newMaaSModelRefUnstructured("HTTP://maas.example.com/model", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_SchemelessEndpoint(t *testing.T) {
	u := newMaaSModelRefUnstructured("maas.example.com/v1/chat/completions", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_SchemelessEndpointWithPort(t *testing.T) {
	u := newMaaSModelRefUnstructured("maas.example.com:8443/v1/chat/completions", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com:8443"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_ProtocolRelativeEndpointWithPort(t *testing.T) {
	u := newMaaSModelRefUnstructured("//maas.example.com:8443/v1/chat/completions", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil || m.URL == nil {
		t.Fatalf("expected non-nil model URL")
	}
	if got, want := m.URL.String(), "https://maas.example.com:8443"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestMaasModelRefToModel_EmptyEndpoint(t *testing.T) {
	u := newMaaSModelRefUnstructured("", nil)
	m := models.MaasModelRefToModel(u)
	if m == nil {
		t.Fatalf("expected non-nil model")
	}
	if m.URL != nil {
		t.Fatalf("URL = %q, want nil", m.URL.String())
	}
}

func TestMaasModelRefToModel_Nil(t *testing.T) {
	if m := models.MaasModelRefToModel(nil); m != nil {
		t.Fatalf("expected nil, got %#v", m)
	}
}
