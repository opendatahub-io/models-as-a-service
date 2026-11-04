package maas

import (
	"strconv"
	"strings"
	"testing"
)

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
		`request.path.startsWith("/v1/model/") && request.path.endsWith("/subscriptions")`,
		`request.path.startsWith("/maas-api/v1/model/") && request.path.endsWith("/subscriptions")`,
		`request.path.startsWith("/maas-api/v1/api-keys")`,
		`request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
		`has(auth.identity.iss)`,
	} {
		if !strings.Contains(predicate, required) {
			t.Errorf("predicate %q does not contain %q", predicate, required)
		}
	}
	if strings.Contains(predicate, "/v1/models") {
		t.Errorf("predicate unexpectedly includes the models route: %q", predicate)
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
	expectedExpression := `{"iss": auth.identity.iss, ` +
		`"sub": auth.identity.sub, ` +
		`"aud": auth.identity.aud, ` +
		`"azp": auth.identity.azp, ` +
		`"name": has(auth.identity.name) ? auth.identity.name : "", ` +
		`"given_name": has(auth.identity.given_name) ? auth.identity.given_name : "", ` +
		`"family_name": has(auth.identity.family_name) ? auth.identity.family_name : "", ` +
		`"preferred_username": has(auth.identity.preferred_username) ? auth.identity.preferred_username : "", ` +
		`"email": has(auth.identity.email) ? auth.identity.email : "", ` +
		`"groups": has(auth.identity.groups) ? auth.identity.groups : [], ` +
		`"https://redhat.com/maas/orgs": "https://redhat.com/maas/orgs" in auth.identity ? auth.identity["https://redhat.com/maas/orgs"] : []}`
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
