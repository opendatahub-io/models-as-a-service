package tenantreconcile_test

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Every tenant applies the maas-api-metrics Service under one name, and istiod pushes
// to every gateway whenever a Service changes. Its render is the same for every tenant
// (TestTenantNeutralOperandsRenderIdenticallyForEveryTenant), so without tenant labels
// tenants applying in turn leave it untouched.
var _ = Describe("The shared maas-api-metrics Service", func() {
	It("drops the tenant labels an earlier release set and is then left untouched", func(ctx SpecContext) {
		appNamespace := pkgtest.NewTestNamespace(ctx, envTest).Name
		gatewayNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		operands := fixture.RenderDefaultTenant(ctx, fixture.InNamespaces(appNamespace, gatewayNamespace))
		i := slices.IndexFunc(operands, func(obj unstructured.Unstructured) bool {
			return obj.GroupVersionKind() == tenantreconcile.GVKService && obj.GetName() == "maas-api-metrics"
		})
		Expect(i).NotTo(Equal(-1))
		service := operands[i : i+1]
		apply := func(tenant client.Object) *unstructured.Unstructured {
			GinkgoHelper()
			Expect(tenantreconcile.ApplyRendered(ctx, envTest.Client, envTest.Environment.Scheme,
				tenant, appNamespace, fixture.ConfigAnchor(), service)).To(Succeed())
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(tenantreconcile.GVKService)
			Expect(envTest.Client.Get(ctx, client.ObjectKeyFromObject(&service[0]), live)).To(Succeed())
			return live
		}

		// Earlier releases applied it with the last applier's tracking labels.
		labeled := service[0].DeepCopy()
		labels := labeled.GetLabels()
		labels[tenantreconcile.LabelTenantName] = "redteam"
		labels[tenantreconcile.LabelTenantNamespace] = "ai-tenant-redteam"
		labeled.SetLabels(labels)
		Expect(envTest.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(labeled),
			client.FieldOwner("maas-controller"), client.ForceOwnership)).To(Succeed())

		applied := apply(fixture.DefaultTenantConfig())
		Expect(applied.GetLabels()).NotTo(HaveKey(tenantreconcile.LabelTenantName))
		Expect(applied.GetLabels()).NotTo(HaveKey(tenantreconcile.LabelTenantNamespace))

		redteam := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-redteam",
			Labels:    map[string]string{tenantreconcile.LabelManagedByAITenant: "true", tenantreconcile.LabelTenantName: "redteam"},
		}}
		Expect(apply(redteam).GetResourceVersion()).To(Equal(applied.GetResourceVersion()))
	})
})
