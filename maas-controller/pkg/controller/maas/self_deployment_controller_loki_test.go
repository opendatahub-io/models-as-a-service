//nolint:testpackage
package maas

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func persesDatasourceFixture(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "perses.dev/v1alpha1",
			"kind":       "PersesDatasource",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "opendatahub",
			},
			"spec": map[string]any{
				"config": map[string]any{
					"plugin": map[string]any{
						"kind": "LokiDatasource",
						"spec": map[string]any{
							"proxy": map[string]any{
								"kind": "HTTPProxy",
								"spec": map[string]any{
									"url": "https://usage-logs-tenancy-proxy.opendatahub.svc:8443",
									"allowedEndpoints": []any{
										map[string]any{"endpointPattern": "/loki/api/v1/query_range", "method": "GET"},
									},
								},
							},
						},
					},
				},
				"client": map[string]any{
					"kubernetesAuth": map[string]any{"enable": true},
					"tls": map[string]any{
						"enable":             true,
						"insecureSkipVerify": false,
						"caCert": map[string]any{
							"type":     "configmap",
							"name":     "usage-logs-tenancy-proxy-ca-bundle",
							"certPath": "service-ca.crt",
						},
					},
				},
			},
		},
	}
}

func TestPatchPersesDatasourceMaaSAPILoki(t *testing.T) {
	g := NewWithT(t)

	t.Run("usage-logs-all gets maas-api URL without only_user", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-all")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"))
	})

	t.Run("usage-logs-multi-tenancy gets maas-api URL with only_user=true", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-multi-tenancy")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs?only_user=true"))
	})

	t.Run("caCert switches to service-ca file and configmap fields are dropped", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-all")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		caCert, found, err := unstructured.NestedMap(ds.Object, "spec", "client", "tls", "caCert")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caCert).To(Equal(map[string]any{
			"type":     "file",
			"certPath": "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt",
		}))

		_, found, err = unstructured.NestedString(ds.Object, "spec", "client", "tls", "caCert", "name")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeFalse(), "configmap caCert name should be removed")
	})

	t.Run("allowedEndpoints keep the /loki prefix on top of the /v1/logs base URL", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-multi-tenancy")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		endpoints, found, err := unstructured.NestedSlice(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "allowedEndpoints")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(endpoints).To(HaveLen(1))
	})

	t.Run("other datasources and kinds are untouched", func(t *testing.T) {
		ds := persesDatasourceFixture("some-other-datasource")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())
		url, _, _ := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(url).To(Equal("https://usage-logs-tenancy-proxy.opendatahub.svc:8443"))

		dep := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"}}
		g.Expect(patchPersesDatasourceMaaSAPILoki(dep)).To(Succeed())
	})
}

func TestLifecycleReconciler_MaasAPILokiDatasourceSwitch(t *testing.T) {
	const (
		infraNS  = "redhat-ai-gateway-infra"
		monNS    = "opendatahub"
		proxyURL = "https://usage-logs-tenancy-proxy.opendatahub.svc:8443"
	)

	persesGV := schema.GroupVersion{Group: "perses.dev", Version: "v1alpha1"}

	newReconciler := func(t *testing.T, withSecret bool) (*LifecycleReconciler, client.Client) {
		t.Helper()
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		builder := newSSAFakeClientBuilder(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg)
		if withSecret {
			builder = builder.WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.LokiReadSecretName,
					Namespace: infraNS,
				},
				Data: map[string][]byte{
					tenantreconcile.LokiReadSecretKey: []byte("https://data-science-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"),
				},
			})
		}
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
			InfraNamespace:        infraNS,
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

	t.Run("secret present switches both datasources to maas-api", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, true)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		admin := getDatasource(t, cl, "usage-logs-all")
		url, found, err := unstructured.NestedString(admin.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"))

		multi := getDatasource(t, cl, "usage-logs-multi-tenancy")
		url, found, err = unstructured.NestedString(multi.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs?only_user=true"))

		caType, found, err := unstructured.NestedString(multi.Object, "spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("file"))
	})

	t.Run("secret absent keeps rendered defaults", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, false)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		multi := getDatasource(t, cl, "usage-logs-multi-tenancy")
		url, found, err := unstructured.NestedString(multi.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(proxyURL))

		caType, found, err := unstructured.NestedString(multi.Object, "spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("configmap"))
	})
}
