package maas

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestAggregateModelSubjectAllowlistsAndGatewaySpec(t *testing.T) {
	const policyNamespace = "models-as-a-service"

	policyA := newMaaSAuthPolicy(
		"policy-a",
		policyNamespace,
		"group-a",
		maasv1alpha1.ModelRef{Name: "model-a", Namespace: "llm"},
		maasv1alpha1.ModelRef{Name: "model-b", Namespace: "llm"},
	)
	policyA.Spec.Subjects.Users = []string{"user-a"}

	policyB := newMaaSAuthPolicy(
		"policy-b",
		policyNamespace,
		"group-b",
		maasv1alpha1.ModelRef{Name: "model-a", Namespace: "llm"},
	)
	policyB.Spec.Subjects.Users = []string{"user-b", "user-a"}

	policyOtherNamespace := newMaaSAuthPolicy(
		"policy-c",
		"other-namespace",
		"group-z",
		maasv1alpha1.ModelRef{Name: "model-a", Namespace: "llm"},
	)
	policyOtherNamespace.Spec.Subjects.Users = []string{"user-z"}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(policyA, policyB, policyOtherNamespace).
		Build()

	r := &MaaSAuthPolicyReconciler{
		Client:           c,
		Scheme:           scheme,
		MaaSAPINamespace: "opendatahub",
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
	}

	allowlists, err := r.aggregateModelSubjectAllowlists(context.Background(), policyNamespace)
	if err != nil {
		t.Fatalf("aggregateModelSubjectAllowlists returned error: %v", err)
	}

	if len(allowlists) != 2 {
		t.Fatalf("expected 2 model allowlist entries, got %d", len(allowlists))
	}

	modelA := allowlists["llm/model-a"]
	if got, want := strings.Join(modelA.Groups, ","), "group-a,group-b"; got != want {
		t.Fatalf("model-a groups = %q, want %q", got, want)
	}
	if got, want := strings.Join(modelA.Users, ","), "user-a,user-b"; got != want {
		t.Fatalf("model-a users = %q, want %q", got, want)
	}

	modelB := allowlists["llm/model-b"]
	if got, want := strings.Join(modelB.Groups, ","), "group-a"; got != want {
		t.Fatalf("model-b groups = %q, want %q", got, want)
	}
	if got, want := strings.Join(modelB.Users, ","), "user-a"; got != want {
		t.Fatalf("model-b users = %q, want %q", got, want)
	}

	allowlistsJSON, err := json.Marshal(allowlists)
	if err != nil {
		t.Fatalf("json.Marshal(allowlists) returned error: %v", err)
	}

	spec := r.buildGatewayAuthPolicySpec(string(allowlistsJSON), nil, false, "", "models-as-a-service", "test-gateway-ns", "test-gateway")
	defaults, ok := spec["defaults"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults block")
	}
	rules, ok := defaults["rules"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults.rules block")
	}
	authorization, ok := rules["authorization"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults.rules.authorization block")
	}
	requireGroupMembership, ok := authorization["require-group-membership"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing require-group-membership rule")
	}
	opa, ok := requireGroupMembership["opa"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing require-group-membership.opa block")
	}
	rego, ok := opa["rego"].(string)
	if !ok {
		t.Fatalf("gateway spec missing require-group-membership.opa.rego string")
	}

	if !strings.Contains(rego, `"llm/model-a":{"users":["user-a","user-b"],"groups":["group-a","group-b"]}`) {
		t.Fatalf("rego does not include aggregated model-a allowlist: %s", rego)
	}
	if !strings.Contains(rego, `"llm/model-b":{"users":["user-a"],"groups":["group-a"]}`) {
		t.Fatalf("rego does not include aggregated model-b allowlist: %s", rego)
	}
}
