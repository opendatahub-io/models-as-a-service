// Package groupname holds the canonical MaaS group-name validation rule for
// maas-api.
package groupname

import "regexp"

// Pattern is the group-name allowlist shared by MaaS components. The same
// literal is embedded in maas-controller's generated gateway expressions; the
// cross-module sync tests fail if the copies drift.
const Pattern = `^[A-Za-z0-9:#._/ -]+$`

var pattern = regexp.MustCompile(Pattern)

// Valid reports whether name is an acceptable MaaS group name.
func Valid(name string) bool {
	return pattern.MatchString(name)
}

// AllValid reports whether every group name is acceptable. An empty list is
// valid, matching the gateway's CEL all(...) behavior.
func AllValid(names []string) bool {
	for _, name := range names {
		if !Valid(name) {
			return false
		}
	}
	return true
}
