// Package groupname holds the canonical MaaS group-name validation rule for
// maas-controller.
//
// The active gateway AuthPolicy and API-key path use this pattern for legacy
// and organization groups. Subscription and model-policy matching remains
// exact after MaaS qualifies an organization group with its organization ID.
// The copies live in independent Go modules, so the mirrored sync tests
// (sync_test.go here, groupname_sync_test.go in maas-api) check them as text
// and fail when they drift. Never edit one without the others:
//
//   - maas-api/internal/api_keys/groupname.go (maas-api canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - docs/content/advanced-administration/external-oidc.md
package groupname

import "regexp"

// Pattern is the legacy group-name allowlist shared by MaaS components.
//
// Allows alphanumerics, colons (system: prefixes), hashes (for IdPs that
// emit names like IAM#AccessGroupId-...), dots, underscores, hyphens,
// spaces, and slashes. Slashes are common in IdP group claims
// (Keycloak full-path groups, Entra ID, Okta, GitHub org/team). Note that
// OpenShift Group objects can never contain '/', ':' or '%' (their names are
// validated as path segments by openshift-apiserver), so this rule sees
// slashes only from OIDC group claims.
const Pattern = `^[A-Za-z0-9:#._/ -]+$`

var pattern = regexp.MustCompile(Pattern)

// Valid reports whether name is an acceptable MaaS group name.
func Valid(name string) bool {
	return pattern.MatchString(name)
}
