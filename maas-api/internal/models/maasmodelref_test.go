package models

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newMaaSModelRefUnstructured(name, namespace, endpoint string, ready bool, hostnames []string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   maasGroup,
		Version: maasVersion,
		Kind:    "MaaSModelRef",
	})
	u.SetName(name)
	u.SetNamespace(namespace)
	u.SetCreationTimestamp(metav1.NewTime(time.Unix(1700000000, 0)))
	_ = unstructured.SetNestedField(u.Object, endpoint, "status", "endpoint")
	if ready {
		_ = unstructured.SetNestedField(u.Object, "Ready", "status", "phase")
	}
	_ = unstructured.SetNestedField(u.Object, "llmisvc", "spec", "modelRef", "kind")
	if len(hostnames) > 0 {
		hostnameInterfaces := make([]interface{}, len(hostnames))
		for i, h := range hostnames {
			hostnameInterfaces[i] = h
		}
		_ = unstructured.SetNestedSlice(u.Object, hostnameInterfaces, "status", "httpRouteHostnames")
	}
	return u
}

func TestMaasModelRefToModel_EndpointFallback_NormalizesToHTTPS(t *testing.T) {
	// When httpRouteHostnames is empty and status.endpoint uses HTTP,
	// the URL should be normalized to HTTPS.
	u := newMaaSModelRefUnstructured("test-model", "default", "http://maas.example.com/test-model", true, nil)
	m := maasModelRefToModel(u)
	if m == nil {
		t.Fatal("maasModelRefToModel returned nil")
	}
	if m.URL == nil {
		t.Fatal("URL is nil")
	}
	got := m.URL.String()
	want := "https://maas.example.com"
	if got != want {
		t.Errorf("URL = %q, want %q (should upgrade HTTP to HTTPS and strip path)", got, want)
	}
}

func TestMaasModelRefToModel_EndpointFallback_StripsPath(t *testing.T) {
	// When httpRouteHostnames is empty and status.endpoint has a path suffix,
	// the URL should expose only the base URL (no path).
	u := newMaaSModelRefUnstructured("test-model", "default", "https://maas.example.com/test-model", true, nil)
	m := maasModelRefToModel(u)
	if m == nil {
		t.Fatal("maasModelRefToModel returned nil")
	}
	if m.URL == nil {
		t.Fatal("URL is nil")
	}
	got := m.URL.String()
	want := "https://maas.example.com"
	if got != want {
		t.Errorf("URL = %q, want %q (should strip path suffix for base URL)", got, want)
	}
}

func TestMaasModelRefToModel_HostnamesPreferred(t *testing.T) {
	// When httpRouteHostnames is present, it should be preferred over status.endpoint
	// and produce an HTTPS base URL.
	u := newMaaSModelRefUnstructured("test-model", "default", "http://internal.svc.local/test-model", true, []string{"maas.example.com"})
	m := maasModelRefToModel(u)
	if m == nil {
		t.Fatal("maasModelRefToModel returned nil")
	}
	if m.URL == nil {
		t.Fatal("URL is nil")
	}
	got := m.URL.String()
	want := "https://maas.example.com"
	if got != want {
		t.Errorf("URL = %q, want %q (should use httpRouteHostnames with HTTPS)", got, want)
	}
}

func TestMaasModelRefToModel_EndpointHTTPS_StripsPath(t *testing.T) {
	// Even when the endpoint already uses HTTPS, the path should be stripped.
	u := newMaaSModelRefUnstructured("test-model", "default", "https://maas.example.com/v1/chat/completions", true, nil)
	m := maasModelRefToModel(u)
	if m == nil {
		t.Fatal("maasModelRefToModel returned nil")
	}
	if m.URL == nil {
		t.Fatal("URL is nil")
	}
	got := m.URL.String()
	want := "https://maas.example.com"
	if got != want {
		t.Errorf("URL = %q, want %q (should strip path even with HTTPS)", got, want)
	}
}

func TestMaasModelRefToModel_EmptyEndpoint(t *testing.T) {
	// When both httpRouteHostnames and status.endpoint are empty, URL should be nil.
	u := newMaaSModelRefUnstructured("test-model", "default", "", true, nil)
	m := maasModelRefToModel(u)
	if m == nil {
		t.Fatal("maasModelRefToModel returned nil")
	}
	if m.URL != nil {
		t.Errorf("URL = %q, want nil (no endpoint available)", m.URL.String())
	}
}

func TestMaasModelRefToModel_Nil(t *testing.T) {
	m := maasModelRefToModel(nil)
	if m != nil {
		t.Error("maasModelRefToModel(nil) should return nil")
	}
}
