package maas

import (
	"strconv"
	"strings"

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

// verifiedIdentityClaims lists the claims carried in the trusted identity
// envelope, in the order they appear in the generated CEL map.
var verifiedIdentityClaims = []struct {
	name       string
	expression string
}{
	// Required provenance and token-binding claims. The authorization rules
	// enforce their configured values before this success response can execute.
	{name: "iss", expression: "auth.identity.iss"},
	{name: "sub", expression: "auth.identity.sub"},
	{name: "aud", expression: "auth.identity.aud"},
	{name: "azp", expression: "auth.identity.azp"},
	// Optional profile claims are display-only. Empty values keep the JSON
	// shape stable for providers that omit them.
	{name: "name", expression: `has(auth.identity.name) ? auth.identity.name : ""`},
	{name: "given_name", expression: `has(auth.identity.given_name) ? auth.identity.given_name : ""`},
	{name: "family_name", expression: `has(auth.identity.family_name) ? auth.identity.family_name : ""`},
	{name: "preferred_username", expression: `has(auth.identity.preferred_username) ? auth.identity.preferred_username : ""`},
	{name: "email", expression: `has(auth.identity.email) ? auth.identity.email : ""`},
	{name: "groups", expression: `has(auth.identity.groups) ? auth.identity.groups : []`},
	{name: "https://redhat.com/maas/orgs", expression: `"https://redhat.com/maas/orgs" in auth.identity ? auth.identity["https://redhat.com/maas/orgs"] : []`},
}

// verifiedIdentityClaimsExpression builds one CEL map literal for the claims
// object. AuthPolicy JSON properties only accept value, selector, or
// expression, so a nested map of per-claim expressions is pruned by the API
// server and the envelope would reach maas-api with empty claims.
func verifiedIdentityClaimsExpression() string {
	entries := make([]string, 0, len(verifiedIdentityClaims))
	for _, claim := range verifiedIdentityClaims {
		entries = append(entries, strconv.Quote(claim.name)+": "+claim.expression)
	}
	return "{" + strings.Join(entries, ", ") + "}"
}

// verifiedIdentityHeader returns Authorino's JSON response configuration for
// the trusted identity transport. The predicate defaults to false so a missing
// OIDC configuration never injects organization claims.
func verifiedIdentityHeader(oidc *oidcConfig, notAPIKeyPredicate string) map[string]any {
	predicate := "false"
	if verifiedIdentityContractConfigured(oidc) {
		predicate = "(" + managementIdentityRoutePredicate + ") && (" + externalOIDCPredicate(notAPIKeyPredicate) + ")"
	}

	return map[string]any{
		"when": []any{
			map[string]any{"predicate": predicate},
		},
		"json": map[string]any{
			"properties": map[string]any{
				"source": map[string]any{"value": "oidc"},
				"claims": map[string]any{"expression": verifiedIdentityClaimsExpression()},
			},
		},
		"metrics":  false,
		"priority": int64(0),
	}
}

func verifiedIdentityContractConfigured(oidc *oidcConfig) bool {
	// Require the core issuer and client binding dimensions. Audience and
	// access-token-purpose checks are optional authorization constraints and
	// must not disable identity propagation for legacy OIDC deployments.
	return oidc != nil &&
		oidc.IssuerURL != "" &&
		oidc.ClientID != ""
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
