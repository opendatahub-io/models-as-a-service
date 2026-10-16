package groupname

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatternCopiesInSync fails when any copy of the group-name allowlist
// drifts from this package's Pattern. The copies live outside this module
// and cannot import groupname, so they are checked as text:
//
//   - maas-api/internal/api_keys/groupname.go (canonical, independent module)
//   - scripts/data/maas-api-authpolicy-external-oidc-patch.yaml (CEL gate)
//   - docs/content/advanced-administration/external-oidc.md (documented charset)
//
// maas-api mirrors this test (internal/api_keys/groupname_sync_test.go), so a
// change to either module's copy fails in the PR that introduces it.
func TestPatternCopiesInSync(t *testing.T) {
	// go test always runs with the working directory set to the package
	// directory, so the checkout root is discoverable without runtime.Caller
	// (whose source paths are module-relative under -trimpath):
	// <repo>/maas-controller/pkg/groupname -> <repo>
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

	apiSrc, ok := read("maas-api/internal/api_keys/groupname.go")
	if !ok {
		t.Skip("maas-api is not checked out alongside maas-controller; nothing to sync against")
	}
	if !strings.Contains(apiSrc, "`"+Pattern+"`") {
		t.Errorf("maas-api/internal/api_keys/groupname.go no longer embeds %q — the group-name rule must stay identical in every copy; edit them together", Pattern)
	}

	yamlSrc, ok := read("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml")
	if !ok {
		t.Fatalf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml is missing; expected in a full-repo checkout")
	}
	if !strings.Contains(yamlSrc, "'"+Pattern+"'") {
		t.Errorf("scripts/data/maas-api-authpolicy-external-oidc-patch.yaml no longer gates on %q — edit the copies together", Pattern)
	}

	docsSrc, ok := read("docs/content/advanced-administration/external-oidc.md")
	if !ok {
		t.Fatalf("docs/content/advanced-administration/external-oidc.md is missing; expected in a full-repo checkout")
	}
	charClass := strings.TrimSuffix(strings.TrimPrefix(Pattern, "^["), "]+$")
	if !strings.Contains(docsSrc, "["+charClass+"]") {
		t.Errorf("docs/content/advanced-administration/external-oidc.md no longer documents the charset %q — edit the copies together", charClass)
	}
}
