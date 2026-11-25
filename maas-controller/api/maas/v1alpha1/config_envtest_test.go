package v1alpha1_test

import (
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config Authorino monitoring default", Ordered, func() {
	BeforeAll(func() {
		_, err := envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join(pkgtest.ProjectRoot(), "deployment", "base", "maas-controller", "crd", "bases", "maas.opendatahub.io_configs.yaml")},
			ErrorIfPathMissing: true,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("defaults on admission and preserves explicit settings", func(ctx SpecContext, enabled *bool, expected bool) {
		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName},
			Spec:       maasv1alpha1.ConfigSpec{AuthorinoMonitoring: enabled},
		}
		Expect(envTest.Create(ctx, cfg, client.DryRunAll)).To(Succeed())
		Expect(cfg.Spec.AuthorinoMonitoring).To(Equal(ptr.To(expected)))
	},
		Entry("omitted", nil, true),
		Entry("enabled", ptr.To(true), true),
		Entry("disabled", ptr.To(false), false),
	)
})
