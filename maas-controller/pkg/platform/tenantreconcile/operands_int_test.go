package tenantreconcile_test

import (
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Tenant operands", func() {
	DescribeTableSubtree("rendered for the default tenant",
		func(opts ...fixture.RenderOption) {
			var (
				appNamespace string
				operands     []unstructured.Unstructured
			)

			BeforeEach(func(ctx SpecContext) {
				appNamespace = pkgtest.NewTestNamespace(ctx, envTest).Name
				gatewayNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
				operands = fixture.RenderDefaultTenant(ctx, append(opts, fixture.InNamespaces(appNamespace, gatewayNamespace))...)
			})

			It("contain no empty lists", func() {
				// The API server stores an empty list as absent. Kinds whose update strategy
				// compares specs literally, NetworkPolicy among them, then bump generation on
				// every apply of unchanged content.
				for _, obj := range operands {
					Expect(fixture.EmptyListPaths(obj)).To(BeEmpty(), "%s %s", obj.GetKind(), obj.GetName())
				}
			})

			When("applied again without changes", func() {
				It("leaves every object untouched", func(ctx SpecContext) {
					// The tenant reconciler watches what it applies, so an object the API server
					// rewrites here re-triggers the reconcile in a loop. Custom resources whose
					// CRDs envtest does not install are stored as sent and cannot move.
					served := slices.DeleteFunc(slices.Clone(operands), func(obj unstructured.Unstructured) bool {
						return !envTest.Serves(obj.GroupVersionKind())
					})
					Expect(served).NotTo(BeEmpty())

					apply := func() {
						Expect(tenantreconcile.ApplyRendered(ctx, envTest.Client, envTest.Environment.Scheme,
							fixture.DefaultTenantConfig(), appNamespace, fixture.ConfigAnchor(), served)).To(Succeed())
					}

					apply()
					applied := fixture.ResourceVersions(ctx, envTest.Client, served)
					apply()

					Expect(changedSince(applied, fixture.ResourceVersions(ctx, envTest.Client, served))).To(BeEmpty(),
						"re-applying unchanged manifests changed these objects")
				})
			})
		},
		Entry("from the OpenShift overlay with bundled Postgres", fixture.WithOverlay("odh"), fixture.WithBundledPostgres()),
		Entry("from the OpenShift overlay with an external database", fixture.WithOverlay("odh")),
		Entry("from the Kubernetes overlay", fixture.WithOverlay("xks"), fixture.WithBundledPostgres()),
	)
})

func changedSince(before, after map[string]string) []string {
	var changed []string
	for key, rv := range before {
		if after[key] != rv {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}
