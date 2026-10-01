// Package fixture sets up the envtest environment for the MaaS controller specs.
package fixture

import (
	"path/filepath"

	"github.com/onsi/ginkgo/v2"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"
)

// SetupTestEnv starts envtest with the controller's CRDs installed and the core and
// MaaS types registered.
func SetupTestEnv() *pkgtest.Client {
	ginkgo.By("Setting up the test environment")
	return pkgtest.Configure(
		pkgtest.WithCRDs(filepath.Join(pkgtest.ProjectRoot(), "deployment", "base", "maas-controller", "crd", "bases")),
		pkgtest.WithScheme(clientgoscheme.AddToScheme, maasv1alpha1.AddToScheme),
	).Start()
}
