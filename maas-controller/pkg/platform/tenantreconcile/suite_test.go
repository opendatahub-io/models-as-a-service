package tenantreconcile_test

import (
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTenantPlatform(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Tenant Platform Suite")
}

var envTest *pkgtest.Client

var _ = BeforeSuite(func() {
	envTest = fixture.SetupTestEnv()
})
