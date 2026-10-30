package api_keys

import (
	"errors"
	"fmt"
	"regexp"
)

// Group-name validation — the canonical MaaS rule enforced by maas-api at
// API-key mint time.
//
// The SAME rule is enforced by maas-controller in the AuthPolicies it
// generates (the oidc-groups-safe Rego rule and the CEL expressions that
// build the X-MaaS-Group header JSON) and by the deploy-time kustomize
// patch. Those copies live in independent Go modules, so the mirrored
// sync tests (groupname_sync_test.go here, sync_test.go in
// maas-controller) check them as text and fail when they drift.
// Never edit one without the others:
//
//   - maas-controller/pkg/groupname/groupname.go (controller canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - maas-controller/pkg/platform/tenantreconcile/postrender.go
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml
//   - docs/content/advanced-administration/external-oidc.md

// validGroupNamePattern matches group names accepted by MaaS.
// Allows alphanumerics, colons (system: prefixes), hashes (for IdPs that
// emit names like IAM#AccessGroupId-...), dots, underscores, hyphens,
// spaces, and slashes. Slashes are common in IdP group claims
// (Keycloak full-path groups, Entra ID, Okta, GitHub org/team) and are safe
// in every downstream sink: Postgres TEXT[] columns, JSON-marshaled bodies,
// and the manual JSON-building CEL expressions in AuthPolicies.
// Rejects control characters, quotes, backslashes, and other unsafe
// characters that could break JSON encoding in AuthPolicy CEL expressions
// (CWE-116/CWE-74 mitigation), plus commas and pipes, which are separators
// in gateway cache-key joins.
var validGroupNamePattern = regexp.MustCompile(`^[A-Za-z0-9:#._/ -]+$`)

// ErrInvalidGroupName is returned by CreateAPIKey when one of the user's
// group names contains characters outside the allowlist. The HTTP handler
// maps it to 400 so callers see a validation error, not a server error.
var ErrInvalidGroupName = errors.New("invalid group name")

// validateGroupNames checks every group name against validGroupNamePattern.
// Groups are stored verbatim in the API-key record and later serialized into
// identity headers by AuthPolicy CEL expressions that build JSON by string
// concatenation (CEL has no JSON escaping), so unsafe characters must be
// rejected at write time.
func validateGroupNames(groups []string) error {
	for _, group := range groups {
		if !validGroupNamePattern.MatchString(group) {
			return fmt.Errorf(
				"%w %q contains invalid characters "+
					"(only alphanumerics, colons, hashes, dots, underscores, hyphens, spaces, and slashes are allowed)",
				ErrInvalidGroupName,
				group,
			)
		}
	}
	return nil
}
