package maas

import (
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/authpolicy"
)

const managementIdentityRoutePredicate = `request.path == "/v1/me" ||
request.path == "/maas-api/v1/me" ||
request.path == "/v1/subscriptions" ||
request.path.startsWith("/v1/subscriptions/") ||
request.path.startsWith("/v1/model/") && request.path.endsWith("/subscriptions") ||
request.path.startsWith("/v1/api-keys") ||
request.path.startsWith("/maas-api/v1/subscriptions") ||
request.path.startsWith("/maas-api/v1/model/") && request.path.endsWith("/subscriptions") ||
request.path.startsWith("/maas-api/v1/api-keys")`

// verifiedIdentityHeader returns Authorino's JSON response configuration for
// the trusted identity transport. The predicate defaults to false so partial
// or legacy configurations never inject organization claims.
func verifiedIdentityHeader(oidc *oidcConfig, notAPIKeyPredicate string) map[string]any {
	predicate := "false"
	if strictIdentityContractConfigured(oidc) {
		predicate = "(" + managementIdentityRoutePredicate + ") && (" + externalOIDCPredicate(notAPIKeyPredicate) + ")"
	}

	return map[string]any{
		"when": []any{
			map[string]any{"predicate": predicate},
		},
		"json": map[string]any{
			"properties": map[string]any{
				"source": map[string]any{"value": "oidc"},
				"claims": map[string]any{
					// Required provenance and token-binding claims. The
					// authorization rules enforce their configured values before
					// this success response can execute.
					"iss": map[string]any{"expression": "auth.identity.iss"},
					"sub": map[string]any{"expression": "auth.identity.sub"},
					"aud": map[string]any{"expression": "auth.identity.aud"},
					"azp": map[string]any{"expression": "auth.identity.azp"},
					// Optional profile claims are display-only. Empty values
					// keep the JSON shape stable for providers that omit them.
					"name": map[string]any{
						"expression": `has(auth.identity.name) ? auth.identity.name : ""`,
					},
					"given_name": map[string]any{
						"expression": `has(auth.identity.given_name) ? auth.identity.given_name : ""`,
					},
					"family_name": map[string]any{
						"expression": `has(auth.identity.family_name) ? auth.identity.family_name : ""`,
					},
					"preferred_username": map[string]any{
						"expression": `has(auth.identity.preferred_username) ? auth.identity.preferred_username : ""`,
					},
					"email": map[string]any{
						"expression": `has(auth.identity.email) ? auth.identity.email : ""`,
					},
					"groups": map[string]any{
						"expression": `has(auth.identity.groups) ? auth.identity.groups : []`,
					},
					"https://redhat.com/maas/orgs": map[string]any{
						"expression": `"https://redhat.com/maas/orgs" in auth.identity ? auth.identity["https://redhat.com/maas/orgs"] : []`,
					},
				},
			},
		},
		"metrics":  false,
		"priority": int64(0),
	}
}

func strictIdentityContractConfigured(oidc *oidcConfig) bool {
	// Do not propagate organization identity unless every binding dimension is
	// configured. Missing issuer, client, audience, or access-token purpose
	// would weaken the trust contract while still carrying claims downstream.
	return oidc != nil &&
		oidc.IssuerURL != "" &&
		oidc.ClientID != "" &&
		oidc.Audience != "" &&
		oidc.AccessTokenPurposeClaim != "" &&
		oidc.AccessTokenPurpose != ""
}

func externalOIDCPredicate(notAPIKeyPredicate string) string {
	// The shared helper is expected to return one conjunction. Fail closed if
	// its shape changes rather than using only the first condition.
	whenClauses := authpolicy.OIDCTokenBoundWhen(notAPIKeyPredicate)
	if len(whenClauses) != 1 {
		return "false"
	}
	clause, ok := whenClauses[0].(map[string]any)
	if !ok {
		return "false"
	}
	predicate, ok := clause["predicate"].(string)
	if !ok || predicate == "" {
		return "false"
	}
	return predicate
}
