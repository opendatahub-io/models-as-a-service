package maas

import (
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/controller/maas/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMaaSControllerSpecs(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MaaS Controller Suite")
}

var envTest *pkgtest.Client

var _ = BeforeSuite(func() {
	envTest = fixture.SetupTestEnv()
})
