//nolint:testpackage
package maas

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

const (
	maasAPILokiDatasourceURL         = "https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"
	maasAPILokiDatasourceOnlyUserURL = maasAPILokiDatasourceURL + "?only_user=true"
)

func datasourceURL(g *WithT, ds *unstructured.Unstructured) string {
	url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	return url
}

// TestLifecycleReconciler_BakedMaaSAPILokiDatasources verifies that the
// dsci-managed usage-log datasources point at the maas-api Loki proxy straight
// from the rendered YAML, with no usage-logs-loki Secret and no runtime
// patching.
func TestLifecycleReconciler_BakedMaaSAPILokiDatasources(t *testing.T) {
	const monNS = "opendatahub"

	persesGV := schema.GroupVersion{Group: "perses.dev", Version: "v1alpha1"}

	newReconciler := func(t *testing.T) (*LifecycleReconciler, client.Client) {
		t.Helper()
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		builder := newSSAFakeClientBuilder(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg)
		// perses.dev is an optional API group; without an explicit mapping the
		// fake client reports NoMatch and ensureUsageLogs skips the datasources.
		mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{persesGV})
		mapper.Add(persesGV.WithKind("PersesDatasource"), apimeta.RESTScopeNamespace)
		mapper.Add(persesGV.WithKind("PersesDashboard"), apimeta.RESTScopeNamespace)
		cl := builder.WithRESTMapper(mapper).Build()

		return &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			UsageLogsManifestPath: lifecycleUsageLogsPath(t),
			MonitoringNamespace:   monNS,
			AppNamespace:          "redhat-ai-gateway-infra",
		}, cl
	}

	getDatasource := func(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
		t.Helper()
		g := NewWithT(t)
		ds := &unstructured.Unstructured{}
		ds.SetGroupVersionKind(persesGV.WithKind("PersesDatasource"))
		ds.SetName(name)
		ds.SetNamespace(monNS)
		g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(ds), ds)).To(Succeed())
		return ds
	}

	t.Run("usage-logs-all is baked with the maas-api proxy URL and file CA", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		admin := getDatasource(t, cl, "usage-logs-all")
		g.Expect(datasourceURL(g, admin)).To(Equal(maasAPILokiDatasourceURL))

		caType, found, err := unstructured.NestedString(admin.Object, "spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("file"))
	})

	t.Run("usage-logs-multi-tenancy is baked with the only_user proxy URL and file CA", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		multi := getDatasource(t, cl, "usage-logs-multi-tenancy")
		g.Expect(datasourceURL(g, multi)).To(Equal(maasAPILokiDatasourceOnlyUserURL))

		caType, found, err := unstructured.NestedString(multi.Object, "spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("file"))
	})
}

// TestUsageLogsDeprecatedOverlayRendersPatchedURLs verifies the deprecated
// usageLogging: true kustomization repoints only the admin datasource to the
// bundle's LokiStack gateway. The user-scoped datasource keeps using maas-api,
// which resolves the legacy usage LokiStack endpoint at runtime.
func TestUsageLogsDeprecatedOverlayRendersPatchedURLs(t *testing.T) {
	g := NewWithT(t)

	resources, err := tenantreconcile.RenderKustomize(lifecycleUsageLogsPath(t), "opendatahub")
	g.Expect(err).NotTo(HaveOccurred())
	for i := range resources {
		if resources[i].GetKind() == "PersesDatasource" {
			g.Expect(patchPersesDatasourceURL(&resources[i], "redhat-ai-gateway-infra")).To(Succeed())
		}
	}

	findDatasource := func(name string) *unstructured.Unstructured {
		for i := range resources {
			if resources[i].GetKind() == "PersesDatasource" && resources[i].GetName() == name {
				return &resources[i]
			}
		}
		g.Fail("datasource not found: " + name)
		return nil
	}

	admin := findDatasource("usage-logs-all")
	g.Expect(datasourceURL(g, admin)).To(Equal("https://usage-gateway-http.opendatahub.svc.cluster.local:8080/api/logs/v1/application"))

	multi := findDatasource("usage-logs-multi-tenancy")
	g.Expect(datasourceURL(g, multi)).To(Equal(maasAPILokiDatasourceOnlyUserURL))
}

// TestLifecycleReconciler_RemovesRetiredTenancyProxy verifies that upgrading
// from the retired Python proxy deletes its controller-managed resources.
func TestLifecycleReconciler_RemovesRetiredTenancyProxy(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}

	builder := newSSAFakeClientBuilder(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg)
	for _, resource := range removedUsageLogsTenancyProxyResources(monitoringNS) {
		stale := resource.DeepCopy()
		stale.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "maas-controller"})
		builder = builder.WithObjects(stale)
	}
	cl := builder.Build()

	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		AppNamespace:          "redhat-ai-gateway-infra",
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
	}
	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

	for _, resource := range removedUsageLogsTenancyProxyResources(monitoringNS) {
		err := cl.Get(context.Background(), client.ObjectKeyFromObject(&resource), resource.DeepCopy())
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %s %s to be removed", resource.GetKind(), resource.GetName())
	}
}
