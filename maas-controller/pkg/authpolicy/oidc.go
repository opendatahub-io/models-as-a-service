package authpolicy

import (
	"fmt"
	"strconv"
)

const externalJWTShape = `request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`

// OIDCTokenBoundWhen selects an externally authenticated JWT while leaving
// OpenShift TokenReview and MaaS API-key identities on their existing paths.
// The top-level iss claim distinguishes an external OIDC claim map from the
// TokenReview user object. An external JWT always comes in the Authorization
// header, and both the JWT match and notAPIKeyPredicate may read it unguarded,
// so the header check comes first rather than relying on which identity
// authenticated a request without it.
func OIDCTokenBoundWhen(notAPIKeyPredicate string) []any {
	return []any{
		map[string]any{
			"predicate": `"authorization" in request.headers && ` + notAPIKeyPredicate + ` && ` + externalJWTShape + ` && has(auth.identity.iss)`,
		},
	}
}

// OIDCTokenBoundPatterns requires the exact issuer and OAuth client. Audience
// and access-token-purpose checks are emitted only when configured, preserving
// legacy OIDC deployments while providing the stricter Grid contract.
func OIDCTokenBoundPatterns(issuerURL, clientID, audience, purposeClaim, purposeValue string) []any {
	patterns := []any{
		map[string]any{
			"selector": "auth.identity.iss",
			"operator": "eq",
			"value":    issuerURL,
		},
		map[string]any{
			"selector": "auth.identity.azp",
			"operator": "eq",
			"value":    clientID,
		},
	}

	if audience != "" {
		// JWT aud may be a string or an array of strings. The predicate picks the
		// comparison by type, so any other aud, or a string that doesn't match, is
		// false instead of a CEL error.
		quotedAudience := strconv.Quote(audience)
		patterns = append(patterns, map[string]any{
			"predicate": fmt.Sprintf(
				`has(auth.identity.aud) && (type(auth.identity.aud) == string ? auth.identity.aud == %s : type(auth.identity.aud) == list && %s in auth.identity.aud)`,
				quotedAudience,
				quotedAudience,
			),
		})
	}

	if purposeClaim != "" && purposeValue != "" {
		patterns = append(patterns, map[string]any{
			"selector": "auth.identity." + purposeClaim,
			"operator": "eq",
			"value":    purposeValue,
		})
	}

	return patterns
}
