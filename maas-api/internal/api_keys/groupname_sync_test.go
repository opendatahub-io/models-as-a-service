package api_keys //nolint:testpackage // Pins the other copies against the private validGroupNamePattern

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGroupNamePatternCopiesInSync is maas-api's mirror of
// maas-controller/pkg/groupname/sync_test.go. It fails when any copy of the
// group-name allowlist drifts from validGroupNamePattern:
//
//   - maas-controller/pkg/groupname/groupname.go (canonical, independent module)
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml (CEL gate)
//   - docs/content/advanced-administration/external-oidc.md (documented charset)
//
// The copies live in an independent Go module and cannot be imported, so they
// are checked as text.
func TestGroupNamePatternCopiesInSync(t *testing.T) {
	// go test always runs with the working directory set to the package
	// directory, so the checkout root is discoverable without runtime.Caller
	// (whose source paths are module-relative under -trimpath):
	// <repo>/maas-api/internal/api_keys -> <repo>
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Dir(filepath.Dir(filepath.Dir(wd)))

	read := func(rel string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b), true
	}

	controllerSrc, ok := read("maas-controller/pkg/groupname/groupname.go")
	if !ok {
		t.Skip("maas-controller is not checked out alongside maas-api; nothing to sync against")
	}
	pattern := validGroupNamePattern.String()
	if !strings.Contains(controllerSrc, "`"+pattern+"`") {
		t.Errorf("maas-controller/pkg/groupname/groupname.go no longer embeds %q — the group-name rule must stay identical in every copy; edit them together", pattern)
	}

	yamlSrc, ok := read("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml")
	if !ok {
		t.Fatalf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml is missing; expected in a full-repo checkout")
	}
	if !strings.Contains(yamlSrc, "'"+pattern+"'") {
		t.Errorf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml no longer gates on %q — edit the copies together", pattern)
	}

	docsSrc, ok := read("docs/content/advanced-administration/external-oidc.md")
	if !ok {
		t.Fatalf("docs/content/advanced-administration/external-oidc.md is missing; expected in a full-repo checkout")
	}
	charClass := strings.TrimSuffix(strings.TrimPrefix(pattern, "^["), "]+$")
	if !strings.Contains(docsSrc, "["+charClass+"]") {
		t.Errorf("docs/content/advanced-administration/external-oidc.md no longer documents the charset %q — edit the copies together", charClass)
	}
}
