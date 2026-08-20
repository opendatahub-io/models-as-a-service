package maas

import (
	"strings"
	"testing"
)

func TestRequireGroupMembershipRegoIsFixedSize(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "opendatahub",
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
	}

	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "test-gateway-ns", "test-gateway")
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

	if !strings.Contains(rego, `accessAllowed`) {
		t.Fatalf("rego does not reference accessAllowed from subscription-info metadata: %s", rego)
	}
	if strings.Contains(rego, `model_access`) {
		t.Fatalf("rego still contains model_access variable (should be removed): %s", rego)
	}
}

func TestRequireGroupMembershipHasWhenGuard(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "opendatahub",
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
	}

	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "test-gateway-ns", "test-gateway")
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

	whenList, ok := requireGroupMembership["when"].([]any)
	if !ok || len(whenList) == 0 {
		t.Fatalf("require-group-membership must have a when guard to skip management endpoints")
	}

	whenEntry, ok := whenList[0].(map[string]any)
	if !ok {
		t.Fatalf("when guard entry is not a map")
	}
	predicate, ok := whenEntry["predicate"].(string)
	if !ok || predicate == "" {
		t.Fatalf("when guard must have a predicate CEL expression")
	}
	if predicate != celModelIdentityAvailable {
		t.Fatalf("when guard predicate = %q, want celModelIdentityAvailable (%q)", predicate, celModelIdentityAvailable)
	}
}

func TestRequireGroupMembershipCacheKey(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "opendatahub",
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
	}

	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "test-gateway-ns", "test-gateway")
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

	cache, ok := requireGroupMembership["cache"].(map[string]any)
	if !ok {
		t.Fatalf("require-group-membership must have cache config")
	}
	key, ok := cache["key"].(map[string]any)
	if !ok {
		t.Fatalf("cache must have key config")
	}
	selector, ok := key["selector"].(string)
	if !ok {
		t.Fatalf("cache key must have selector")
	}

	expectedSelector := subscriptionGatewayCacheKeySelector()
	if selector != expectedSelector {
		t.Fatalf("cache key selector = %q, want %q (subscriptionGatewayCacheKeySelector)", selector, expectedSelector)
	}
}
