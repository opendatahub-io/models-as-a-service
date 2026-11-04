package api_keys

import (
	"errors"
	"fmt"
	"regexp"
)

// Group-name validation for legacy identities at API-key mint time.
//
// The same rule is enforced by maas-controller in the AuthPolicies it
// generates and by the deploy-time kustomize patch. Flat organization
// groups use exact matching and do not use this allowlist. The copies live
// in independent Go modules, so the mirrored
// sync tests (groupname_sync_test.go here, sync_test.go in
// maas-controller) check them as text and fail when they drift.
// Never edit one without the others:
//
//   - maas-controller/pkg/groupname/groupname.go (controller canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - maas-controller/pkg/platform/tenantreconcile/postrender.go
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml
//   - docs/content/advanced-administration/external-oidc.md

// validGroupNamePattern matches legacy group names accepted by MaaS.
// Allows alphanumerics, colons (system: prefixes), hashes (for IdPs that
// emit names like IAM#AccessGroupId-...), dots, underscores, hyphens,
// spaces, and slashes. Slashes are common in IdP group claims
// (Keycloak full-path groups, Entra ID, Okta, GitHub org/team).
// Flat organization groups bypass this pattern and use safe JSON encoding.
var validGroupNamePattern = regexp.MustCompile(`^[A-Za-z0-9:#._/ -]+$`)

// ErrInvalidGroupName is returned by CreateAPIKey when one of the user's
// group names contains characters outside the allowlist. The HTTP handler
// maps it to 400 so callers see a validation error, not a server error.
var ErrInvalidGroupName = errors.New("invalid group name")

// validateGroupNames applies the legacy allowlist.
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
