package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveApplicationsNamespace(t *testing.T) {
	cases := []struct {
		name           string
		flag           string
		applicationsNS string
		maasAPINS      string
		want           string
	}{
		{name: "flag overrides env", flag: "custom-ns", applicationsNS: "from-env", maasAPINS: "from-downward", want: "custom-ns"},
		{name: "applications env", flag: "", applicationsNS: "from-env", maasAPINS: "", want: "from-env"},
		{name: "downward api env", flag: "", applicationsNS: "", maasAPINS: "from-downward-api", want: "from-downward-api"},
		{name: "empty", flag: "", applicationsNS: "", maasAPINS: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(applicationsNamespaceEnv, tc.applicationsNS)
			t.Setenv("MAAS_API_NAMESPACE", tc.maasAPINS)
			if got := resolveApplicationsNamespace(tc.flag); got != tc.want {
				t.Fatalf("resolveApplicationsNamespace(%q) = %q, want %q", tc.flag, got, tc.want)
			}
		})
	}
}

func TestEnsureAITenantNamespaceWithClientCreatesNamespace(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	if err := ensureAITenantNamespaceWithClient(context.Background(), "redhat-ai-gateway-infra", clientset); err != nil {
		t.Fatalf("ensure AITenant namespace: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "redhat-ai-gateway-infra", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get AITenant namespace: %v", err)
	}
	if got := ns.Labels["opendatahub.io/generated-namespace"]; got != "true" {
		t.Fatalf("generated namespace label = %q, want true", got)
	}
	if got := ns.Labels["app.kubernetes.io/managed-by"]; got != "maas-controller" {
		t.Fatalf("managed-by label = %q, want maas-controller", got)
	}
}
