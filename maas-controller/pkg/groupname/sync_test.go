package groupname

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestPatternCopiesInSync fails when any copy of the group-name allowlist
// drifts from this package's Pattern. The copies live outside this module
// and cannot import groupname, so they are checked as text:
//
//   - maas-api/internal/api_keys/groupname.go (canonical, independent module)
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml (CEL gate)
//   - docs/content/advanced-administration/external-oidc.md (documented charset)
//
// maas-api mirrors this test (internal/api_keys/groupname_sync_test.go), so a
// change to either module's copy fails in the PR that introduces it.
func TestPatternCopiesInSync(t *testing.T) {
	// go test always runs with the working directory set to the package
	// directory, so the checkout root is discoverable without runtime.Caller
	// (whose source paths are module-relative under -trimpath):
	// <repo>/maas-controller/pkg/groupname -> <repo>
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))

	read := func(rel string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b), true
	}

	apiSrc, ok := read("maas-api/internal/api_keys/groupname.go")
	if !ok {
		t.Skip("maas-api is not checked out alongside maas-controller; nothing to sync against")
	}
	if !strings.Contains(apiSrc, "`"+Pattern+"`") {
		t.Errorf("maas-api/internal/api_keys/groupname.go no longer embeds %q — the group-name rule must stay identical in every copy; edit them together", Pattern)
	}

	yamlSrc, ok := read("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml")
	if !ok {
		t.Fatalf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml is missing; expected in a full-repo checkout")
	}
	if !strings.Contains(yamlSrc, "'"+Pattern+"'") {
		t.Errorf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml no longer gates on %q — edit the copies together", Pattern)
	}

	docsSrc, ok := read("docs/content/advanced-administration/external-oidc.md")
	if !ok {
		t.Fatalf("docs/content/advanced-administration/external-oidc.md is missing; expected in a full-repo checkout")
	}
	charClass := strings.TrimSuffix(strings.TrimPrefix(Pattern, "^["), "]+$")
	if !strings.Contains(docsSrc, "["+charClass+"]") {
		t.Errorf("docs/content/advanced-administration/external-oidc.md no longer documents the charset %q — edit the copies together", charClass)
	}
}

func TestExternalOIDCPatchKeepsFlatGroupsOutOfLegacyHeaderJSON(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))
	yamlSrc, err := os.ReadFile(filepath.Join(repo, "scripts/data/maas-api-authpolicy-external-oidc-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	source := string(yamlSrc)
	if !strings.Contains(source, "auth.metadata.apiKeyValidation.groupsJSON") {
		t.Error("external OIDC patch must use the JSON-encoded API-key group list")
	}
	if !strings.Contains(source, `"org_id" in auth.identity ? [] :`) {
		t.Error("flat OIDC groups must not be serialized into the legacy group header")
	}
	if strings.Contains(source, `auth.identity.groups.map(g, auth.identity["org_id"] + "/" + g)`) {
		t.Error("flat OIDC groups must be qualified by maas-api from the verified envelope")
	}
}

func TestExternalOIDCPatchGroupExpressionBehavior(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))
	yamlSrc, err := os.ReadFile(filepath.Join(repo, "scripts/data/maas-api-authpolicy-external-oidc-patch.yaml"))
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, yaml.Unmarshal(yamlSrc, &document))
	spec, ok := document["spec"].(map[string]any)
	require.True(t, ok)
	rules, ok := spec["rules"].(map[string]any)
	require.True(t, ok)
	response, ok := rules["response"].(map[string]any)
	require.True(t, ok)
	success, ok := response["success"].(map[string]any)
	require.True(t, ok)
	headers, ok := success["headers"].(map[string]any)
	require.True(t, ok)
	groupHeader, ok := headers["X-MaaS-Group"].(map[string]any)
	require.True(t, ok)
	plain, ok := groupHeader["plain"].(map[string]any)
	require.True(t, ok)
	expression, ok := plain["expression"].(string)
	require.True(t, ok)

	const organizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	tests := []struct {
		name     string
		identity map[string]any
		want     string
	}{
		{
			name: "organization-bound OIDC uses the identity envelope",
			identity: map[string]any{
				"iss":    "https://idp.example.com/realms/test",
				"org_id": organizationID,
				"groups": []any{"ai-grid-admins"},
			},
			want: `[]`,
		},
		{
			name: "opaque organization group is not serialized into the legacy header",
			identity: map[string]any{
				"iss":    "https://idp.example.com/realms/test",
				"org_id": organizationID,
				"groups": []any{"quote\"group", "backslash\\group", "équipe,ops|team", " padded "},
			},
			want: `[]`,
		},
		{
			name: "legacy OIDC group with slash stays plain",
			identity: map[string]any{
				"iss":    "https://idp.example.com/realms/test",
				"groups": []any{"users/team"},
			},
			want: `["system:authenticated","users/team"]`,
		},
		{
			name: "unbound OIDC qualified group is rejected",
			identity: map[string]any{
				"iss":    "https://idp.example.com/realms/test",
				"groups": []any{organizationID + "/reserved-ai-grid-admins"},
			},
			want: `["system:authenticated"]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateExternalOIDCGroupExpression(t, expression, test.identity)
			require.Equal(t, test.want, got)
		})
	}
}

func TestExternalOIDCPatchAPIKeyGroupExpressionBehavior(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))
	yamlSrc, err := os.ReadFile(filepath.Join(repo, "scripts/data/maas-api-authpolicy-external-oidc-patch.yaml"))
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, yaml.Unmarshal(yamlSrc, &document))
	spec, _ := document["spec"].(map[string]any)
	rules, _ := spec["rules"].(map[string]any)
	response, _ := rules["response"].(map[string]any)
	success, _ := response["success"].(map[string]any)
	headers, _ := success["headers"].(map[string]any)
	group, _ := headers["X-MaaS-Group"].(map[string]any)
	plain, _ := group["plain"].(map[string]any)
	expression, _ := plain["expression"].(string)

	tests := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{
			name: "API key with empty snapshot stays empty",
			metadata: map[string]any{"apiKeyValidation": map[string]any{
				"groups":     []any{},
				"groupsJSON": `[]`,
			}},
			want: `[]`,
		},
		{
			name: "API key uses its saved JSON groups",
			metadata: map[string]any{"apiKeyValidation": map[string]any{
				"groups":     []any{"org-id/group"},
				"groupsJSON": `["org-id/group"]`,
			}},
			want: `["org-id/group"]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateExternalOIDCGroupExpressionWithAuth(t, expression, map[string]any{"metadata": test.metadata})
			require.Equal(t, test.want, got)
		})
	}
}

func TestExternalOIDCPatchCarriesExactIdentityContract(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))
	yamlSrc, err := os.ReadFile(filepath.Join(repo, "scripts/data/maas-api-authpolicy-external-oidc-patch.yaml"))
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, yaml.Unmarshal(yamlSrc, &document))
	spec, _ := document["spec"].(map[string]any)
	rules, _ := spec["rules"].(map[string]any)
	response, _ := rules["response"].(map[string]any)
	success, _ := response["success"].(map[string]any)
	headers, _ := success["headers"].(map[string]any)

	groupHeader, _ := headers["X-MaaS-Group"].(map[string]any)
	groupPlain, _ := groupHeader["plain"].(map[string]any)
	groupExpression, _ := groupPlain["expression"].(string)
	require.Contains(t, groupExpression, "auth.metadata.apiKeyValidation.groupsJSON")

	identityHeader, _ := headers["X-MaaS-Identity"].(map[string]any)
	identityJSON, _ := identityHeader["json"].(map[string]any)
	identityProperties, _ := identityJSON["properties"].(map[string]any)
	claims, _ := identityProperties["claims"].(map[string]any)
	claimsExpression, _ := claims["expression"].(string)
	require.Contains(t, claimsExpression, `"org_id"`)
	require.Contains(t, claimsExpression, `"roles"`)
	require.NotContains(t, claimsExpression, `https://redhat.com/maas/orgs`)

	const organizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	groups := []any{"quote\"group", "backslash\\group", "équipe,ops|team", " padded ", "\x00"}
	gotClaims := evaluateExternalOIDCClaimsExpression(t, claimsExpression, map[string]any{
		"iss":      "https://idp.example.com/realms/test",
		"sub":      "grid-user",
		"aud":      []any{"maas"},
		"azp":      "maas-client",
		"org_id":   organizationID,
		"org_name": "Example",
		"roles":    []any{"admins"},
		"groups":   groups,
	})
	require.Equal(t, groups, gotClaims["groups"])
	require.Equal(t, organizationID, gotClaims["org_id"])
	require.Equal(t, []any{"admins"}, gotClaims["roles"])

	authorization, _ := rules["authorization"].(map[string]any)
	groupValidation, _ := authorization["oidc-groups-safe"].(map[string]any)
	groupValidationOPA, _ := groupValidation["opa"].(map[string]any)
	groupValidationRego, _ := groupValidationOPA["rego"].(string)
	require.Contains(t, groupValidationRego, "count(groups) > 128")
	require.Contains(t, groupValidationRego, "base64.encode(g)")
	require.Contains(t, groupValidationRego, "count(encoded) > 344")
	require.Contains(t, groupValidationRego, `not regex.match("==$", encoded)`)
	require.Contains(t, groupValidationRego, `regex.match("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/", g)`)
	denyHeaders, _ := authorization["deny-client-identity-headers"].(map[string]any)
	denyPatternsMap, _ := denyHeaders["patternMatching"].(map[string]any)
	denyPatterns, _ := denyPatternsMap["patterns"].([]any)
	var deniesExactHeader bool
	for _, pattern := range denyPatterns {
		patternMap, _ := pattern.(map[string]any)
		predicate := patternMap["predicate"]
		if predicate == `!("x-maas-exact-groups" in request.headers)` {
			deniesExactHeader = true
		}
	}
	require.True(t, deniesExactHeader)
}

func evaluateExternalOIDCGroupExpression(t *testing.T, expression string, identity map[string]any) string {
	t.Helper()
	return evaluateExternalOIDCGroupExpressionWithAuth(t, expression, map[string]any{"identity": identity})
}

func evaluateExternalOIDCGroupExpressionWithAuth(t *testing.T, expression string, auth map[string]any) string {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), ext.Strings())
	require.NoError(t, err)
	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)
	value, _, err := program.Eval(map[string]any{"auth": auth})
	require.NoError(t, err)
	result, ok := value.Value().(string)
	require.True(t, ok, "group expression result = %#v, want string", value.Value())
	return result
}

func evaluateExternalOIDCClaimsExpression(t *testing.T, expression string, identity map[string]any) map[string]any {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), ext.Strings())
	require.NoError(t, err)
	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err())
	program, err := env.Program(ast)
	require.NoError(t, err)
	value, _, err := program.Eval(map[string]any{"auth": map[string]any{"identity": identity}})
	require.NoError(t, err)
	native, err := value.ConvertToNative(reflect.TypeFor[map[string]any]())
	require.NoError(t, err)
	claims, ok := native.(map[string]any)
	require.True(t, ok)
	return claims
}
