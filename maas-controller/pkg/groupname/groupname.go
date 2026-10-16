// Package groupname holds the canonical MaaS group-name validation rule for
// maas-controller.
//
// The pattern is embedded into the CEL and Rego expressions of the generated
// gateway AuthPolicy (the oidc-groups-safe rule and the X-MaaS-Group header
// JSON builders) and into the per-tenant maas-api AuthPolicy post-render.
// maas-api enforces the same rule at API-key mint time. The copies live in
// independent Go modules, so the mirrored sync tests (sync_test.go here,
// groupname_sync_test.go in maas-api) check them as text and fail when
// they drift. Never edit one without the others:
//
//   - maas-api/internal/api_keys/groupname.go (maas-api canonical)
//   - maas-controller/pkg/controller/maas/maasauthpolicy_controller.go
//   - maas-controller/pkg/platform/tenantreconcile/postrender.go
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml
//   - docs/content/advanced-administration/external-oidc.md
package groupname

import "regexp"

// Pattern is the group-name allowlist shared by all MaaS components.
//
// Allows alphanumerics, colons (system: prefixes), hashes (for IdPs that
// emit names like IAM#AccessGroupId-...), dots, underscores, hyphens,
// spaces, and slashes. Slashes are common in IdP group claims
// (Keycloak full-path groups, Entra ID, Okta, GitHub org/team) and are safe
// in every downstream sink: Postgres TEXT[] columns, JSON-marshaled bodies,
// and the manual JSON-building CEL expressions in AuthPolicies. Note that
// OpenShift Group objects can never contain '/', ':' or '%' (their names are
// validated as path segments by openshift-apiserver), so this rule only ever
// sees slashes from OIDC group claims.
//
// Rejects control characters, quotes, backslashes, and other unsafe
// characters that could break JSON encoding in AuthPolicy CEL expressions
// (CWE-116/CWE-74 mitigation), plus commas and pipes, which are separators
// in gateway cache-key joins.
const Pattern = `^[A-Za-z0-9:#._/ -]+$`

var pattern = regexp.MustCompile(Pattern)

// Valid reports whether name is an acceptable MaaS group name.
func Valid(name string) bool {
	return pattern.MatchString(name)
}
