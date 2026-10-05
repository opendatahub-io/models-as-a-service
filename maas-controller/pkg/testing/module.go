package testing

import (
	"context"
	"os/exec"
	"strings"

	"github.com/onsi/gomega"
)

// ModuleDir returns the directory holding module at the version go.mod resolves, for
// specs that install CRDs shipped by a dependency. The CRDs then follow dependency
// bumps without being copied into this tree.
func ModuleDir(ctx context.Context, module string) string {
	out, err := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "go list -m %s", module)

	dir := strings.TrimSpace(string(out))
	gomega.Expect(dir).NotTo(gomega.BeEmpty(), "%s is not in the module cache - run go mod download", module)
	return dir
}
