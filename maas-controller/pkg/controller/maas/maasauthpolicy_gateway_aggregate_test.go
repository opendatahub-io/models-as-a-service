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

// The identity filter reads subscription-info, which is only fetched when a model
// identity is available. Unguarded, it fails on every /maas-api request, and
// Authorino cancels the other priority-0 response configs evaluated alongside it,
// so API-key requests reach maas-api without some or all X-MaaS-* headers.
func TestIdentityFilterHasModelIdentityGuard(t *testing.T) {
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
	response, ok := rules["response"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults.rules.response block")
	}
	success, ok := response["success"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults.rules.response.success block")
	}
	filters, ok := success["filters"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing defaults.rules.response.success.filters block")
	}
	identity, ok := filters["identity"].(map[string]any)
	if !ok {
		t.Fatalf("gateway spec missing identity filter")
	}

	whenList, ok := identity["when"].([]any)
	if !ok || len(whenList) == 0 {
		t.Fatalf("identity filter must have a when guard to skip requests without subscription-info")
	}
	whenEntry, ok := whenList[0].(map[string]any)
	if !ok {
		t.Fatalf("when guard entry is not a map")
	}
	if predicate, _ := whenEntry["predicate"].(string); predicate != celModelIdentityAvailable {
		t.Fatalf("when guard predicate = %q, want celModelIdentityAvailable (%q)", predicate, celModelIdentityAvailable)
	}
}
