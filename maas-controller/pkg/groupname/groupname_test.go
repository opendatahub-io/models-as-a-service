package groupname

import "testing"

// Canonical MaaS group-name examples — keep in sync with
// maas-api/internal/api_keys/service_test.go (TestCreateAPIKey_GroupNameValidation).
// These same examples must be accepted by the maas-api API-key mint path
// (validGroupNamePattern) and by the AuthPolicy expressions that embed
// groupname.Pattern.
func TestPatternCanonicalExamples(t *testing.T) {
	valid := []string{
		"system:authenticated",
		"my-group",
		"group with spaces",
		"group.with.dots",
		"group_with_underscores",
		"GROUP123",
		"a:b:c",
		"mix-of_all.types:123",
		"IAM#AccessGroupId-123",
		"a#b",
		"/Engineering",      // Keycloak full-path style
		"dev/team-a",        // path-style group (Entra ID, Okta, GitHub org/team)
		"a/b/c:d.e_f-g h i", // every allowed character class together
	}
	for _, name := range valid {
		if !Valid(name) {
			t.Errorf("group name %q should be valid", name)
		}
	}

	invalid := []string{
		`group"with"quotes`,
		`group\with\backslash`,
		"group\nwith\nnewline",
		"group\twith\ttab",
		"group;with;semicolon",
		"group,with,comma",
		"group[with]brackets",
		"group{with}braces",
		"group(with)parens",
		"group=with=equals",
		"group$with$dollar",
		"group@with@at",
		"group%with%percent",
	}
	for _, name := range invalid {
		if Valid(name) {
			t.Errorf("group name %q should be rejected", name)
		}
	}
}
