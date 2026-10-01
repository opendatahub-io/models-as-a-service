package tenantreconcile_test

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Every tenant renders the shared operands under the same names with the same content.
// Each tenant's reconcile applies them and watches them, so any write one tenant's apply
// makes to them wakes the others, which write them back.
var _ = Describe("Shared tenant operands", func() {
	var (
		appNamespace    string
		defaultOperands []unstructured.Unstructured
		redOperands     []unstructured.Unstructured
		shared          []unstructured.Unstructured
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest).Name
		gatewayNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		in := fixture.InNamespaces(appNamespace, gatewayNamespace)
		defaultOperands = servedOnly(fixture.RenderTenant(ctx, fixture.DefaultTenantConfig(), in))
		redOperands = servedOnly(fixture.RenderTenant(ctx, fixture.TenantConfig("red"), in))
		shared = slices.DeleteFunc(slices.Clone(defaultOperands), func(obj unstructured.Unstructured) bool {
			return obj.GetLabels()[tenantreconcile.LabelSharedOperand] != "true"
		})
		Expect(shared).NotTo(BeEmpty())
	})

	apply := func(ctx context.Context, objs []unstructured.Unstructured) {
		GinkgoHelper()
		Expect(tenantreconcile.ApplyRendered(ctx, envTest.Client, envTest.Environment.Scheme,
			appNamespace, fixture.ConfigAnchor(), objs)).To(Succeed())
	}

	When("a second tenant applies them after the first", func() {
		It("changes none of them, and the first tenant's next apply changes nothing either", func(ctx SpecContext) {
			apply(ctx, defaultOperands)
			applied := fixture.ResourceVersions(ctx, envTest.Client, defaultOperands)

			// The first tenant's per-tenant objects are in the comparison too: the second
			// tenant's apply must not reach them either.
			apply(ctx, redOperands)
			Expect(changedSince(applied, fixture.ResourceVersions(ctx, envTest.Client, defaultOperands))).To(BeEmpty(),
				"the second tenant's apply rewrote these objects")

			apply(ctx, defaultOperands)
			Expect(changedSince(applied, fixture.ResourceVersions(ctx, envTest.Client, defaultOperands))).To(BeEmpty(),
				"the first tenant's re-apply rewrote these objects")
		})
	})

	When("the previous controller applied the tenant's operands with its tracking labels", func() {
		BeforeEach(func(ctx SpecContext) {
			fixture.ApplyTrackingLabelled(ctx, envTest.Client, envTest.Environment.Scheme,
				fixture.DefaultTenantConfig(), appNamespace, defaultOperands)
		})

		It("relabels each shared object once and leaves the per-tenant ones alone, after which a second tenant changes nothing", func(ctx SpecContext) {
			perTenant := slices.DeleteFunc(slices.Clone(defaultOperands), func(obj unstructured.Unstructured) bool {
				return obj.GetLabels()[tenantreconcile.LabelSharedOperand] == "true"
			})
			Expect(perTenant).NotTo(BeEmpty())
			labelled := fixture.ResourceVersions(ctx, envTest.Client, shared)
			perTenantBefore := fixture.ResourceVersions(ctx, envTest.Client, perTenant)
			perTenantLabels := make([]map[string]string, len(perTenant))
			for i, obj := range perTenant {
				perTenantLabels[i] = liveLabels(ctx, obj)
			}

			// Server-side apply drops the tracking labels: the same field manager owns them
			// and no longer sends them.
			apply(ctx, defaultOperands)
			Expect(changedSince(labelled, fixture.ResourceVersions(ctx, envTest.Client, shared))).To(HaveLen(len(shared)))
			for _, obj := range shared {
				Expect(liveLabels(ctx, obj)).To(And(
					HaveKeyWithValue(tenantreconcile.LabelSharedOperand, "true"),
					Not(HaveKey(tenantreconcile.LabelTenantName)),
					Not(HaveKey(tenantreconcile.LabelTenantNamespace)),
				), "%s %s", obj.GetKind(), obj.GetName())
			}
			// The per-tenant objects, the workloads among them, carry the same tracking
			// labels either way, so the upgrade writes none of them and rolls no pods.
			Expect(changedSince(perTenantBefore, fixture.ResourceVersions(ctx, envTest.Client, perTenant))).To(BeEmpty(),
				"the upgrade rewrote these per-tenant objects")
			for i, obj := range perTenant {
				Expect(liveLabels(ctx, obj)).To(Equal(perTenantLabels[i]), "%s %s", obj.GetKind(), obj.GetName())
			}

			relabelled := fixture.ResourceVersions(ctx, envTest.Client, shared)
			apply(ctx, redOperands)
			Expect(changedSince(relabelled, fixture.ResourceVersions(ctx, envTest.Client, shared))).To(BeEmpty(),
				"the second tenant's apply rewrote these shared objects")
		})
	})
})

// servedOnly drops custom resources whose CRDs envtest does not install; the platform
// apply would fail on them.
func servedOnly(objs []unstructured.Unstructured) []unstructured.Unstructured {
	return slices.DeleteFunc(objs, func(obj unstructured.Unstructured) bool {
		return !envTest.Serves(obj.GroupVersionKind())
	})
}

func liveLabels(ctx context.Context, obj unstructured.Unstructured) map[string]string {
	GinkgoHelper()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	Expect(envTest.Get(ctx, client.ObjectKeyFromObject(&obj), live)).To(Succeed())
	return live.GetLabels()
}
