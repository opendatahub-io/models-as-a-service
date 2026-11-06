package authpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func TestOIDCTokenBoundPatterns(t *testing.T) {
	tests := []struct {
		name         string
		audience     string
		purposeClaim string
		purposeValue string
		want         []any
	}{
		{
			name: "legacy contract requires issuer and client only",
			want: []any{
				map[string]any{"selector": "auth.identity.iss", "operator": "eq", "value": "https://issuer.example.com"},
				map[string]any{"selector": "auth.identity.azp", "operator": "eq", "value": "maas-api"},
			},
		},
		{
			name:         "strict contract adds audience and token purpose",
			audience:     "maas-api",
			purposeClaim: "token_use",
			purposeValue: "access",
			want: []any{
				map[string]any{"selector": "auth.identity.iss", "operator": "eq", "value": "https://issuer.example.com"},
				map[string]any{"selector": "auth.identity.azp", "operator": "eq", "value": "maas-api"},
				map[string]any{
					"predicate": `has(auth.identity.aud) && (auth.identity.aud == "maas-api" || "maas-api" in auth.identity.aud)`,
				},
				map[string]any{"selector": "auth.identity.token_use", "operator": "eq", "value": "access"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := OIDCTokenBoundPatterns(
				"https://issuer.example.com",
				"maas-api",
				test.audience,
				test.purposeClaim,
				test.purposeValue,
			)

			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("OIDCTokenBoundPatterns() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestOIDCTokenBoundWhen(t *testing.T) {
	got := OIDCTokenBoundWhen(`!request.headers.authorization.startsWith("Bearer sk-oai-")`)
	if len(got) != 1 {
		t.Fatalf("OIDCTokenBoundWhen() length = %d, want 1", len(got))
	}

	predicate, ok := got[0].(map[string]any)["predicate"].(string)
	if !ok {
		t.Fatalf("OIDCTokenBoundWhen() predicate is not a string: %#v", got[0])
	}
	for _, required := range []string{
		`!request.headers.authorization.startsWith("Bearer sk-oai-")`,
		`request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
		`has(auth.identity.iss)`,
	} {
		if !strings.Contains(predicate, required) {
			t.Errorf("predicate %q does not contain %q", predicate, required)
		}
	}
}
