package maas

import (
	"strings"
	"testing"
)

func TestVerifiedIdentityHeaderRequiresStrictOIDCContract(t *testing.T) {
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
			if when["predicate"] != "false" {
				t.Fatalf("predicate = %v, want false", when["predicate"])
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
	if _, exists := claims["groups"]; exists {
		t.Errorf("verified identity envelope unexpectedly includes caller groups")
	}
	for _, claim := range []string{
		"iss", "sub", "aud", "azp", "name", "given_name", "family_name",
		"preferred_username", "email", "https://redhat.com/maas/orgs",
	} {
		if _, exists := claims[claim]; !exists {
			t.Errorf("claims is missing %q", claim)
		}
	}
}
