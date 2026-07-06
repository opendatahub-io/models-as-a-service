package maas

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	. "github.com/onsi/gomega"
)

func TestSupplementalConfigRBACManifestSupportsControllerCaches(t *testing.T) {
	g := NewWithT(t)

	configRole := readClusterRoleManifest(t, "clusterrole_maas_configs.yaml")
	g.Expect(hasUnrestrictedRule(configRole.Rules, "configs", "list", "watch")).To(BeTrue())
	g.Expect(hasNamedRule(configRole.Rules, "configs", "default", "get", "patch", "update", "delete")).To(BeTrue())
	g.Expect(hasWildcardResource(configRole.Rules)).To(BeFalse())
	g.Expect(hasDangerousVerb(configRole.Rules)).To(BeFalse())
}

func readClusterRoleManifest(t *testing.T, name string) rbacv1.ClusterRole {
	t.Helper()
	var role rbacv1.ClusterRole
	readRBACManifest(t, name, &role)
	return role
}

func readRBACManifest(t *testing.T, name string, out any) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-controller", "rbac", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read RBAC manifest %s: %v", name, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		t.Fatalf("decode RBAC manifest %s: %v", name, err)
	}
}

func hasUnrestrictedRule(rules []rbacv1.PolicyRule, resource string, verbs ...string) bool {
	return hasUnrestrictedRuleForGroup(rules, "maas.opendatahub.io", resource, verbs...)
}

func hasUnrestrictedRuleForGroup(rules []rbacv1.PolicyRule, apiGroup, resource string, verbs ...string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) == 0 &&
			rbacManifestContains(rule.APIGroups, apiGroup) &&
			rbacManifestContains(rule.Resources, resource) &&
			containsAll(rule.Verbs, verbs) {
			return true
		}
	}
	return false
}

func hasNamedRule(rules []rbacv1.PolicyRule, resource, resourceName string, verbs ...string) bool {
	for _, rule := range rules {
		if rbacManifestContains(rule.APIGroups, "maas.opendatahub.io") &&
			rbacManifestContains(rule.Resources, resource) &&
			rbacManifestContains(rule.ResourceNames, resourceName) &&
			containsAll(rule.Verbs, verbs) {
			return true
		}
	}
	return false
}

func containsAll(got []string, want []string) bool {
	for _, item := range want {
		if !rbacManifestContains(got, item) {
			return false
		}
	}
	return true
}

func rbacManifestContains(items []string, want string) bool {
	return slices.Contains(items, want)
}

func hasWildcardResource(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if rbacManifestContains(rule.Resources, "*") {
			return true
		}
	}
	return false
}

func hasDangerousVerb(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		for _, verb := range []string{"bind", "escalate", "impersonate"} {
			if rbacManifestContains(rule.Verbs, verb) || rbacManifestContains(rule.Verbs, "*") {
				return true
			}
		}
	}
	return false
}
