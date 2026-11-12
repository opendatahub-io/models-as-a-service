package maas

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

const identityTestOrganizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"

func TestVerifiedIdentityClaimsExpressionCarriesFlatOrganizationClaims(t *testing.T) {
	identity := verifiedIdentityClaimsForTest()
	identity["groups"] = []any{"users"}
	identity["org_id"] = identityTestOrganizationID
	identity["org_name"] = "coke"
	identity["roles"] = []any{"admins"}

	claims := evaluateVerifiedIdentityClaims(t, identity)
	if claims["org_id"] != identityTestOrganizationID {
		t.Errorf("org_id = %#v, want %q", claims["org_id"], identityTestOrganizationID)
	}
	if claims["org_name"] != "coke" {
		t.Errorf("org_name = %#v, want coke", claims["org_name"])
	}
	roles, ok := claims["roles"].([]any)
	if !ok || len(roles) != 1 || roles[0] != "admins" {
		t.Errorf("roles = %#v, want [admins]", claims["roles"])
	}
	if groups, ok := claims["groups"].([]any); !ok || len(groups) != 1 || groups[0] != "users" {
		t.Errorf("groups = %#v, want [users]", claims["groups"])
	}
	if _, found := claims["https://redhat.com/maas/orgs"]; found {
		t.Fatal("verified claims contain the removed nested organization claim")
	}
}

func TestVerifiedIdentityClaimsExpressionAllowsMissingOptionalOrganizationClaims(t *testing.T) {
	identity := verifiedIdentityClaimsForTest()
	identity["org_id"] = identityTestOrganizationID

	claims := evaluateVerifiedIdentityClaims(t, identity)
	if claims["org_id"] != identityTestOrganizationID {
		t.Errorf("org_id = %#v, want %q", claims["org_id"], identityTestOrganizationID)
	}
	if claims["org_name"] != "" {
		t.Errorf("org_name = %#v, want an empty string", claims["org_name"])
	}
	roles, ok := claims["roles"].([]any)
	if !ok || len(roles) != 0 {
		t.Errorf("roles = %#v, want an empty list", claims["roles"])
	}
}

func TestVerifiedIdentityClaimsExpressionDoesNotMapOrganizationClaimsWithoutOrgID(t *testing.T) {
	identity := verifiedIdentityClaimsForTest()
	identity["org_name"] = "unrelated-name"
	identity["roles"] = []any{"admins"}

	claims := evaluateVerifiedIdentityClaims(t, identity)
	for _, name := range []string{"org_id", "org_name", "roles", "https://redhat.com/maas/orgs"} {
		if _, found := claims[name]; found {
			t.Errorf("non-organization identity contains organization claim %q", name)
		}
	}
}

func TestVerifiedIdentityClaimsExpressionPreservesMalformedPresentOrganizationClaims(t *testing.T) {
	identity := verifiedIdentityClaimsForTest()
	identity["org_id"] = int64(42)
	identity["org_name"] = false
	identity["roles"] = "admins"

	claims := evaluateVerifiedIdentityClaims(t, identity)
	if claims["org_id"] != int64(42) {
		t.Errorf("org_id = %#v, want the original numeric value", claims["org_id"])
	}
	if claims["org_name"] != false {
		t.Errorf("org_name = %#v, want the original boolean value", claims["org_name"])
	}
	if claims["roles"] != "admins" {
		t.Errorf("roles = %#v, want the original string value", claims["roles"])
	}
}

func verifiedIdentityClaimsForTest() map[string]any {
	return map[string]any{
		"iss": "https://issuer.example.com",
		"sub": "user-1",
		"aud": "maas-api",
		"azp": "maas-client",
	}
}

func evaluateVerifiedIdentityClaims(t *testing.T, identity map[string]any) map[string]any {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType))
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, issues := env.Compile(verifiedIdentityClaimsExpression())
	if issues.Err() != nil {
		t.Fatalf("Compile: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program: %v", err)
	}
	value, _, err := program.Eval(map[string]any{"auth": map[string]any{"identity": identity}})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	native, err := value.ConvertToNative(reflect.TypeFor[map[string]any]())
	if err != nil {
		t.Fatalf("ConvertToNative: %v", err)
	}
	claims, ok := native.(map[string]any)
	if !ok {
		t.Fatalf("claims value = %#v, want map[string]any", native)
	}
	return claims
}

func TestVerifiedIdentityHeaderRequiresCoreOIDCContract(t *testing.T) {
	tests := []struct {
		name string
		oidc *oidcConfig
	}{
		{name: "no oidc", oidc: nil},
		{name: "missing issuer", oidc: &oidcConfig{
			ClientID:                "maas-client",
			Audience:                "maas-api",
			AccessTokenPurposeClaim: "token_use",
			AccessTokenPurpose:      "access",
		}},
		{name: "missing client", oidc: &oidcConfig{
			IssuerURL:               "https://issuer.example.com",
			Audience:                "maas-api",
			AccessTokenPurposeClaim: "token_use",
			AccessTokenPurpose:      "access",
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := verifiedIdentityHeader(test.oidc, `!request.headers.authorization.startsWith("Bearer sk-oai-")`)
			whenItems, ok := header["when"].([]any)
			if !ok || len(whenItems) == 0 {
				t.Fatalf("when conditions are missing: %#v", header)
			}
			when, ok := whenItems[0].(map[string]any)
			if !ok {
				t.Fatalf("first when condition is not an object: %#v", whenItems[0])
			}
			if when["predicate"] != "false" {
				t.Fatalf("predicate = %v, want false", when["predicate"])
			}
		})
	}
}

func TestVerifiedIdentityHeaderWithoutOptionalOIDCContract(t *testing.T) {
	tests := []struct {
		name string
		oidc *oidcConfig
	}{
		{name: "legacy issuer and client only", oidc: &oidcConfig{
			IssuerURL: "https://issuer.example.com",
			ClientID:  "maas-client",
		}},
		{name: "missing audience", oidc: &oidcConfig{
			IssuerURL:               "https://issuer.example.com",
			ClientID:                "maas-client",
			AccessTokenPurposeClaim: "token_use",
			AccessTokenPurpose:      "access",
		}},
		{name: "missing purpose", oidc: &oidcConfig{
			IssuerURL: "https://issuer.example.com",
			ClientID:  "maas-client",
			Audience:  "maas-api",
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := verifiedIdentityHeader(test.oidc, `!request.headers.authorization.startsWith("Bearer sk-oai-")`)
			whenItems, ok := header["when"].([]any)
			if !ok || len(whenItems) == 0 {
				t.Fatalf("when conditions are missing: %#v", header)
			}
			when, ok := whenItems[0].(map[string]any)
			if !ok {
				t.Fatalf("first when condition is not an object: %#v", whenItems[0])
			}
			if predicate, ok := when["predicate"].(string); !ok || predicate == "false" {
				t.Fatalf("predicate = %v, want an enabled OIDC predicate", when["predicate"])
			}
		})
	}
}

func TestVerifiedIdentityHeaderStrictContract(t *testing.T) {
	oidc := &oidcConfig{
		IssuerURL:               "https://issuer.example.com",
		ClientID:                "maas-client",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
	}
	notAPIKey := `!request.headers.authorization.startsWith("Bearer sk-oai-")`

	header := verifiedIdentityHeader(oidc, notAPIKey)
	whenItems, ok := header["when"].([]any)
	if !ok || len(whenItems) == 0 {
		t.Fatalf("when conditions are missing: %#v", header)
	}
	when, ok := whenItems[0].(map[string]any)
	if !ok {
		t.Fatalf("first when condition is not an object: %#v", whenItems[0])
	}
	predicate, ok := when["predicate"].(string)
	if !ok {
		t.Fatalf("predicate is not a string: %#v", when["predicate"])
	}
	for _, required := range []string{
		`request.path == "/v1/me"`,
		`request.path == "/v1/models"`,
		`request.path == "/maas-api/v1/models"`,
		`request.path.startsWith("/v1/model/") && request.path.endsWith("/subscriptions")`,
		`request.path.startsWith("/maas-api/v1/model/") && request.path.endsWith("/subscriptions")`,
		`request.path.startsWith("/maas-api/v1/api-keys")`,
		`request.path.startsWith("/v1/logs")`,
		`request.path.startsWith("/maas-api/v1/logs")`,
		`request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
		`has(auth.identity.iss)`,
	} {
		if !strings.Contains(predicate, required) {
			t.Errorf("predicate %q does not contain %q", predicate, required)
		}
	}
	jsonResponse, ok := header["json"].(map[string]any)
	if !ok {
		t.Fatalf("json response is missing: %#v", header)
	}
	properties, ok := jsonResponse["properties"].(map[string]any)
	if !ok {
		t.Fatalf("json properties are missing: %#v", jsonResponse)
	}
	source, ok := properties["source"].(map[string]any)
	if !ok {
		t.Fatalf("source is missing: %#v", properties)
	}
	if source["value"] != "oidc" {
		t.Errorf("source = %#v, want oidc", source["value"])
	}
	claims, ok := properties["claims"].(map[string]any)
	if !ok {
		t.Fatalf("claims are missing: %#v", properties)
	}
	// The claims object must be a single CEL expression. A nested map of
	// per-claim expressions is not a valid AuthPolicy JSON property and the API
	// server prunes it to an empty object.
	if len(claims) != 1 {
		t.Fatalf("claims must only carry an expression: %#v", claims)
	}
	expression, ok := claims["expression"].(string)
	if !ok {
		t.Fatalf("claims expression is not a string: %#v", claims)
	}
	expectedBaseClaims := `{"iss": auth.identity.iss, ` +
		`"sub": auth.identity.sub, ` +
		`"aud": auth.identity.aud, ` +
		`"azp": auth.identity.azp, ` +
		`"name": has(auth.identity.name) ? auth.identity.name : "", ` +
		`"given_name": has(auth.identity.given_name) ? auth.identity.given_name : "", ` +
		`"family_name": has(auth.identity.family_name) ? auth.identity.family_name : "", ` +
		`"preferred_username": has(auth.identity.preferred_username) ? auth.identity.preferred_username : "", ` +
		`"email": has(auth.identity.email) ? auth.identity.email : "", ` +
		`"groups": has(auth.identity.groups) ? auth.identity.groups : []}`
	expectedOrganizationClaims := strings.TrimSuffix(expectedBaseClaims, "}") +
		`, "org_id": auth.identity["org_id"], ` +
		`"org_name": "org_name" in auth.identity ? auth.identity["org_name"] : "", ` +
		`"roles": "roles" in auth.identity ? auth.identity["roles"] : []}`
	expectedExpression := `"org_id" in auth.identity ? ` + expectedOrganizationClaims + ` : ` + expectedBaseClaims
	if expression != expectedExpression {
		t.Errorf("claims expression = %q, want %q", expression, expectedExpression)
	}
}

// TestGatewayAuthPolicyJSONPropertiesMatchAuthPolicySchema guards against
// response JSON properties that the AuthPolicy CRD would silently prune. Each
// property value may only carry value, selector, or expression.
func TestGatewayAuthPolicyJSONPropertiesMatchAuthPolicySchema(t *testing.T) {
	oidc := &oidcConfig{
		IssuerURL:               "https://issuer.example.com",
		ClientID:                "maas-client",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
	}
	for _, test := range []struct {
		name string
		oidc *oidcConfig
	}{
		{name: "without oidc", oidc: nil},
		{name: "strict oidc", oidc: oidc},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := gatewayAuthPolicySpecTestObject(t, test.oidc)
			found := assertJSONPropertiesMatchAuthPolicySchema(t, spec.Object, "")
			if found == 0 {
				t.Fatal("no JSON response properties found in gateway AuthPolicy spec")
			}
		})
	}
}

// assertJSONPropertiesMatchAuthPolicySchema walks a generated AuthPolicy and
// returns the number of JSON response properties it checked.
func assertJSONPropertiesMatchAuthPolicySchema(t *testing.T, node any, path string) int {
	t.Helper()
	checked := 0
	switch value := node.(type) {
	case map[string]any:
		if jsonResponse, ok := value["json"].(map[string]any); ok {
			if properties, ok := jsonResponse["properties"].(map[string]any); ok {
				for name, property := range properties {
					propertyPath := path + ".json.properties." + name
					config, ok := property.(map[string]any)
					if !ok {
						t.Errorf("%s is not an object: %#v", propertyPath, property)
						continue
					}
					for key := range config {
						if key != "value" && key != "selector" && key != "expression" {
							t.Errorf("%s has field %q; AuthPolicy JSON properties only allow value, selector, or expression", propertyPath, key)
						}
					}
					checked++
				}
			}
		}
		for key, child := range value {
			checked += assertJSONPropertiesMatchAuthPolicySchema(t, child, path+"."+key)
		}
	case []any:
		for i, child := range value {
			checked += assertJSONPropertiesMatchAuthPolicySchema(t, child, path+"["+strconv.Itoa(i)+"]")
		}
	}
	return checked
}
