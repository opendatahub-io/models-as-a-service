package maas

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		InfraNamespace:   "opendatahub",
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

func TestAggregateModelSubjectAllowlistsModelNameAliases(t *testing.T) {
	const policyNamespace = "models-as-a-service"

	policy := newMaaSAuthPolicy(
		"policy-a",
		policyNamespace,
		"group-a",
		maasv1alpha1.ModelRef{Name: "model-a", Namespace: "llm"},
	)
	policy.Spec.Subjects.Users = []string{"user-a"}

	// MaaSModelRef with Status.ResolvedModelAlias set — resolveHeaderModelKeys
	// reads this to add body-routed alias keys into the aggregate.
	modelRef := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: "llm"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: "model-a"},
		},
		Status: maasv1alpha1.MaaSModelStatus{
			ResolvedModelAlias: "claude-opus-4-8",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(policy, modelRef).
		Build()

	r := &MaaSAuthPolicyReconciler{
		Client:           c,
		Scheme:           scheme,
		InfraNamespace:   "opendatahub",
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
	}

	allowlists, err := r.aggregateModelSubjectAllowlists(context.Background(), policyNamespace)
	if err != nil {
		t.Fatalf("aggregateModelSubjectAllowlists returned error: %v", err)
	}

	alias, ok := allowlists["claude-opus-4-8"]
	if !ok {
		t.Fatalf("expected alias entry for claude-opus-4-8, got keys: %v", keysOf(allowlists))
	}
	if got, want := strings.Join(alias.Groups, ","), "group-a"; got != want {
		t.Fatalf("alias groups = %q, want %q", got, want)
	}
	if got, want := strings.Join(alias.Users, ","), "user-a"; got != want {
		t.Fatalf("alias users = %q, want %q", got, want)
	}

	// Canonical namespace/name entry must be unaffected by aliasing.
	if got, want := strings.Join(allowlists["llm/model-a"].Users, ","), "user-a"; got != want {
		t.Fatalf("llm/model-a users = %q, want %q", got, want)
	}
}

func keysOf(m map[string]modelSubjectAllowlist) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
