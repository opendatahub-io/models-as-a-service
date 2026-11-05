package api_keys

import (
	"errors"
	"fmt"
	"regexp"
)

// Group-name validation for all authenticated groups.
//
// The same rule is enforced by maas-controller in the AuthPolicies it
// generates. MaaS applies the rule
// to legacy and organization groups alike, so every authenticated group can
// be stored in an API-key snapshot. The copies live in independent Go
// modules, so the mirrored
// sync tests (groupname_sync_test.go here, sync_test.go in
// maas-controller) check them as text and fail when they drift.
// Never edit one without the others:
//
//   - maas-controller/pkg/groupname/groupname.go (controller canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - docs/content/advanced-administration/external-oidc.md

// validGroupNamePattern matches legacy group names accepted by MaaS.
// Allows alphanumerics, colons (system: prefixes), hashes (for IdPs that
// emit names like IAM#AccessGroupId-...), dots, underscores, hyphens,
// spaces, and slashes. Slashes are common in IdP group claims
// (Keycloak full-path groups, Entra ID, Okta, GitHub org/team).
// Organization groups that do not match this pattern are rejected before
// authentication completes.
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
