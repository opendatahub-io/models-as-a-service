/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"sigs.k8s.io/yaml"
)

// RHCL 1.4.2 / Authorino v1.4.2 rejects has() on request.headers map keys (subscript or field access) (RHOAIENG-84699).
var forbiddenHasOnRequestHeaders = regexp.MustCompile(`has\s*\(\s*request\.headers(?:\s*\[|\.)`)

type authPolicyCELOccurrence struct {
	Path string
	Expr string
}

func collectAuthPolicyCEL(path string, v any) []authPolicyCELOccurrence {
	switch x := normalizeYAMLValue(v).(type) {
	case nil:
		return nil
	case map[string]any:
		var out []authPolicyCELOccurrence
		for k, val := range x {
			child := joinCELPath(path, k)
			if k == "predicate" || k == "expression" {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, authPolicyCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			if k == "selector" {
				if s, ok := val.(string); ok && strings.Contains(s, "request.") {
					out = append(out, authPolicyCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			out = append(out, collectAuthPolicyCEL(child, val)...)
		}
		return out
	case []any:
		var out []authPolicyCELOccurrence
		for i, item := range x {
			out = append(out, collectAuthPolicyCEL(fmt.Sprintf("%s[%d]", path, i), item)...)
		}
		return out
	default:
		return nil
	}
}

func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = val
		}
		return m
	case map[string]any:
		return x
	case []any:
		return x
	default:
		return v
	}
}

func joinCELPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func assertRHCLCompatibleCEL(t *testing.T, path, expr string) {
	t.Helper()
	if forbiddenHasOnRequestHeaders.MatchString(expr) {
		t.Fatalf("CEL at %s uses has(request.headers...) which Authorino v1.4.2 rejects; use (\"header\" in request.headers) instead: %q", path, expr)
	}
}

func TestRejectRHCLHasOnRequestHeadersCEL(t *testing.T) {
	for _, expr := range []string{
		`!has(request.headers["x-maas-username"])`,
		`has (request.headers.authorization)`,
		`has( request.headers.foo)`,
	} {
		if !forbiddenHasOnRequestHeaders.MatchString(expr) {
			t.Errorf("expected rejection for %q", expr)
		}
	}
}

func assertAllAuthPolicyCEL_RHCLCompatible(t *testing.T, spec map[string]any) {
	t.Helper()
	occurrences := collectAuthPolicyCEL("spec", spec)
	if len(occurrences) == 0 {
		t.Fatal("expected at least one CEL expression in AuthPolicy spec")
	}
	for _, occ := range occurrences {
		assertRHCLCompatibleCEL(t, occ.Path, occ.Expr)
	}
}

func TestGatewayAuthPolicySpec_AllCEL_RHCLCompatible(t *testing.T) {
	sampleOIDC := &oidcConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-client",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
		TTL:                     300,
	}

	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "maas-system",
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "gateway-ns",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
		AuthzCacheTTL:    60,
	}

	cases := []struct {
		name           string
		oidc           *oidcConfig
		xAPIKeyEnabled bool
		tenantID       string
		tenantName     string
		gatewayNS      string
		gatewayName    string
	}{
		{
			name:           "default_gateway",
			oidc:           nil,
			xAPIKeyEnabled: false,
			tenantName:     "models-as-a-service",
			gatewayNS:      "test-gateway-ns",
			gatewayName:    "test-gateway",
		},
		{
			name:           "oidc_enabled",
			oidc:           sampleOIDC,
			xAPIKeyEnabled: false,
			tenantName:     "models-as-a-service",
			gatewayNS:      "test-gateway-ns",
			gatewayName:    "test-gateway",
		},
		{
			name:           "x_api_key_enabled",
			oidc:           nil,
			xAPIKeyEnabled: true,
			tenantName:     "models-as-a-service",
			gatewayNS:      "gateway-ns",
			gatewayName:    "maas-default-gateway",
		},
		{
			name:           "tenant_gateway",
			oidc:           nil,
			xAPIKeyEnabled: false,
			tenantID:       "tenant-abc",
			tenantName:     "tenant-ns",
			gatewayNS:      "tenant-gw-ns",
			gatewayName:    "tenant-gateway",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := r.buildGatewayAuthPolicySpec(
				tc.oidc, tc.xAPIKeyEnabled, tc.tenantID, tc.tenantName, tc.gatewayNS, tc.gatewayName, nil,
			)
			assertAllAuthPolicyCEL_RHCLCompatible(t, spec)
		})
	}
}

func TestCELGroupsQualifyFlatOIDCGroups(t *testing.T) {
	const (
		organizationA = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
		organizationB = "1e0ce6d3-3812-4dab-a090-1c8eacdddb4e"
	)

	tests := []struct {
		name     string
		identity map[string]any
		metadata map[string]any
		want     []any
	}{
		{
			name:     "organization A",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"users"}},
			want:     []any{organizationA + "/users"},
		},
		{
			name:     "organization B",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationB, "groups": []any{"users"}},
			want:     []any{organizationB + "/users"},
		},
		{
			name:     "group with slash",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"users/team"}},
			want:     []any{organizationA + "/users/team"},
		},
		{
			name:     "group case is preserved",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"Users"}},
			want:     []any{organizationA + "/Users"},
		},
		{
			name:     "group trailing space is preserved",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"users "}},
			want:     []any{organizationA + "/users "},
		},
		{
			name:     "flat group at 256 ASCII bytes is accepted",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{strings.Repeat("a", 256)}},
			want:     []any{organizationA + "/" + strings.Repeat("a", 256)},
		},
		{
			name:     "flat group outside the group-name allowlist is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"équipe"}},
			want:     []any{},
		},
		{
			name:     "non-string flat group is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{42}},
			want:     []any{},
		},
		{
			name:     "empty flat group list",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{}},
			want:     []any{},
		},
		{
			name:     "flat identity without groups",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA},
			want:     []any{},
		},
		{
			name:     "empty group is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{""}},
			want:     []any{},
		},
		{
			name:     "duplicate flat group is qualified and passed through (rego will reject)",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"users", "users"}},
			want:     []any{organizationA + "/users", organizationA + "/users"},
		},
		{
			name:     "organization-qualified flat group is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{organizationA + "/users"}},
			want:     []any{},
		},
		{
			name:     "more than 128 flat groups are rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": makeGroups(129)},
			want:     []any{},
		},
		{
			name:     "invalid organization id is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": "not-a-uuid", "groups": []any{"users"}},
			want:     []any{},
		},
		{
			name:     "non-organization OIDC groups stay plain",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{"users"}},
			want:     []any{"users"},
		},
		{
			name:     "non-string legacy OIDC group is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{42}},
			want:     []any{},
		},
		{
			name:     "legacy OIDC group with slash stays plain",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{"users/team"}},
			want:     []any{"users/team"},
		},
		{
			name:     "organization-qualified OIDC group without organization is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{organizationA + "/users"}},
			want:     []any{},
		},
		{
			name:     "Kubernetes groups stay plain",
			identity: map[string]any{"user": map[string]any{"groups": []any{"users"}}},
			want:     []any{"users"},
		},
		{
			name:     "organization-qualified Kubernetes group is rejected",
			identity: map[string]any{"user": map[string]any{"groups": []any{organizationA + "/users"}}},
			want:     []any{},
		},
		{
			name:     "API key snapshot stays unchanged",
			identity: map[string]any{"org_id": organizationA},
			metadata: map[string]any{"apiKeyValidation": map[string]any{"groups": []any{organizationA + "/users"}}},
			want:     []any{organizationA + "/users"},
		},
		{
			name:     "explicit system group is qualified",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"system:authenticated"}},
			want:     []any{organizationA + "/system:authenticated"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := map[string]any{"identity": test.identity}
			if test.metadata != nil {
				auth["metadata"] = test.metadata
			}
			got := evaluateGatewayCELList(t, celGroups, auth)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("celGroups = %#v, want %#v", got, test.want)
			}
		})
	}
}

func makeGroups(count int) []any {
	groups := make([]any, 0, count)
	for i := 0; i < count; i++ {
		groups = append(groups, fmt.Sprintf("group-%d", i))
	}
	return groups
}

func TestGatewayAuthPolicyFlatGroupsAllowAllowlistedValues(t *testing.T) {
	const organizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	groups := []any{"users", "team/devs", "system:authenticated", "role#admin", "app.example", "test_group", "dash-name", "with space"}
	auth := map[string]any{"identity": map[string]any{
		"iss":    "https://issuer.example.com",
		"org_id": organizationID,
		"groups": groups,
	}}
	want := []any{
		organizationID + "/users",
		organizationID + "/team/devs",
		organizationID + "/system:authenticated",
		organizationID + "/role#admin",
		organizationID + "/app.example",
		organizationID + "/test_group",
		organizationID + "/dash-name",
		organizationID + "/with space",
	}
	got := evaluateGatewayCELList(t, celGroups, auth)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("celGroups = %#v, want %#v", got, want)
	}
}

func TestCELAPIKeyGroupsHeaderJSONFallsBackToGroups(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{
			name:     "groupsJSON is preferred",
			metadata: map[string]any{"apiKeyValidation": map[string]any{"groupsJSON": `["saved"]`, "groups": []any{"ignored"}}},
			want:     `["saved"]`,
		},
		{
			name:     "groups list is used when groupsJSON is absent",
			metadata: map[string]any{"apiKeyValidation": map[string]any{"groups": []any{"users", "admins"}}},
			want:     `["users","admins"]`,
		},
		{
			name:     "empty groups list is encoded",
			metadata: map[string]any{"apiKeyValidation": map[string]any{"groups": []any{}}},
			want:     `[]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := map[string]any{"metadata": test.metadata}
			got := evaluateGatewayCELString(t, celAPIKeyGroupsHeaderJSON, auth)
			if got != test.want {
				t.Fatalf("celAPIKeyGroupsHeaderJSON = %s, want %s", got, test.want)
			}
		})
	}
}

func TestCELTokenGroupsHeaderJSONReservesQualifiedGroups(t *testing.T) {
	const organizationA = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"

	tests := []struct {
		name     string
		identity map[string]any
		want     string
	}{
		{
			name:     "organization-bound OIDC group is qualified",
			identity: map[string]any{"iss": "https://issuer.example.com", "org_id": organizationA, "groups": []any{"users"}},
			want:     `["system:authenticated","` + organizationA + `/users"]`,
		},
		{
			name:     "legacy OIDC group with slash stays plain",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{"users/team"}},
			want:     `["system:authenticated","users/team"]`,
		},
		{
			name:     "unbound OIDC qualified group is rejected",
			identity: map[string]any{"iss": "https://issuer.example.com", "groups": []any{organizationA + "/users"}},
			want:     `["system:authenticated"]`,
		},
		{
			name:     "legacy Kubernetes group with slash stays plain",
			identity: map[string]any{"user": map[string]any{"groups": []any{"users/team"}}},
			want:     `["system:authenticated","users/team"]`,
		},
		{
			name:     "unbound Kubernetes qualified group is rejected",
			identity: map[string]any{"user": map[string]any{"groups": []any{organizationA + "/users"}}},
			want:     `["system:authenticated"]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateGatewayCELString(t, celTokenGroupsHeaderJSON, map[string]any{"identity": test.identity})
			if got != test.want {
				t.Errorf("celTokenGroupsHeaderJSON = %s, want %s", got, test.want)
			}
		})
	}
}

func evaluateGatewayCELList(t *testing.T, expression string, auth map[string]any) []any {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), cel.Variable("request", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("Compile: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program: %v", err)
	}
	value, _, err := program.Eval(map[string]any{
		"auth":    auth,
		"request": map[string]any{"path": "/v1/models", "headers": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	native, err := value.ConvertToNative(reflect.TypeFor[[]any]())
	if err != nil {
		t.Fatalf("ConvertToNative: %v", err)
	}
	groups, ok := native.([]any)
	if !ok {
		t.Fatalf("groups value = %#v, want []any", native)
	}
	return groups
}

func evaluateGatewayCELString(t *testing.T, expression string, auth map[string]any) string {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), cel.Variable("request", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("Compile: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program: %v", err)
	}
	value, _, err := program.Eval(map[string]any{
		"auth":    auth,
		"request": map[string]any{"path": "/v1/model/default/llm", "headers": map[string]any{"x-maas-subscription": "gold"}},
	})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	result, ok := value.Value().(string)
	if !ok {
		t.Fatalf("value = %#v, want string", value.Value())
	}
	return result
}

func evaluateGatewayCELMap(t *testing.T, expression string, auth map[string]any) map[string]any {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), cel.Variable("request", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("Compile: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program: %v", err)
	}
	value, _, err := program.Eval(map[string]any{
		"auth":    auth,
		"request": map[string]any{"path": "/v1/model/default/llm", "headers": map[string]any{"x-maas-subscription": "gold"}},
	})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	native, err := value.ConvertToNative(reflect.TypeFor[map[string]any]())
	if err != nil {
		t.Fatalf("ConvertToNative: %v", err)
	}
	body, ok := native.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v, want map[string]any", native)
	}
	return body
}

func TestMaaSAPIAuthPolicyYAML_CEL_RHCLCompatible(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-api", "policies", "auth-policy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth-policy.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode auth-policy.yaml: %v", err)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		// yaml.v3 may decode nested maps as map[interface{}]interface{}
		if m, ok := doc["spec"].(map[any]any); ok {
			spec = make(map[string]any, len(m))
			for k, v := range m {
				spec[fmt.Sprint(k)] = v
			}
		}
	}
	if spec == nil {
		t.Fatal("auth-policy.yaml missing spec map")
	}
	assertAllAuthPolicyCEL_RHCLCompatible(t, spec)
}
