package api_keys

import (
	"errors"
	"fmt"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/groupname"
)

// Group-name validation for all authenticated groups.
//
// The same rule is enforced by maas-controller in the AuthPolicies it
// generates. Flat organization groups use exact matching and are checked
// against the same rule, so every authenticated group can be stored in an
// API-key snapshot. The copies live in independent Go modules, so the mirrored
// sync tests (groupname_sync_test.go here, sync_test.go in
// maas-controller) check them as text and fail when they drift.
// Never edit one without the others:
//
//   - maas-controller/pkg/groupname/groupname.go (controller canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - docs/content/advanced-administration/external-oidc.md

// ErrInvalidGroupName is returned by CreateAPIKey when one of the user's
// group names contains characters outside the allowlist. The HTTP handler
// maps it to 400 so callers see a validation error, not a server error.
var ErrInvalidGroupName = errors.New("invalid group name")

// validateGroupNames checks every group name against the canonical rule.
// Groups are stored verbatim in the API-key record and later serialized into
// identity headers by AuthPolicy CEL expressions that build JSON by string
// concatenation (CEL has no JSON escaping), so unsafe characters must be
// rejected at write time.
func validateGroupNames(groups []string) error {
	for _, group := range groups {
		if !groupname.Valid(group) {
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
