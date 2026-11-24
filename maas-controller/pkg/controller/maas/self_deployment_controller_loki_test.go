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
	"k8s.io/client-go/tools/events"
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

func TestLifecycleReconciler_UsageLogsProxyAnnotation(t *testing.T) {
	const (
		infraNS  = "redhat-ai-gateway-infra"
		monNS    = "opendatahub"
		proxyURL = "https://usage-logs-tenancy-proxy.opendatahub.svc:8443"
	)

	persesGV := schema.GroupVersion{Group: "perses.dev", Version: "v1alpha1"}

	newReconciler := func(t *testing.T, mode string, withSecret bool, recorder events.EventRecorder) (*LifecycleReconciler, client.Client) {
		t.Helper()
		s := lifecycleTestScheme(t)

		annotations := map[string]string{}
		if mode != "" {
			annotations[tenantreconcile.UsageLogsProxyAnnotation] = mode
		}
		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{
				Name:        maasv1alpha1.ConfigInstanceName,
				UID:         types.UID("cfg-uid"),
				Annotations: annotations,
			},
		}

		// perses.dev is an optional API group; without an explicit mapping the
		// fake client reports NoMatch and ensureUsageLogs skips the datasources.
		mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{persesGV})
		mapper.Add(persesGV.WithKind("PersesDatasource"), apimeta.RESTScopeNamespace)
		mapper.Add(persesGV.WithKind("PersesDashboard"), apimeta.RESTScopeNamespace)
		builder := newSSAFakeClientBuilder(s).
			WithStatusSubresource(&maasv1alpha1.Config{}).
			WithObjects(cfg)
		if withSecret {
			builder = builder.WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tenantreconcile.LokiReadSecretName,
					Namespace: infraNS,
				},
				Data: map[string][]byte{
					tenantreconcile.LokiReadSecretKey: []byte("https://data-science-usage-logs-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"),
				},
			})
		}
		cl := builder.WithRESTMapper(mapper).Build()

		return &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			UsageLogsManifestPath: lifecycleUsageLogsPath(t),
			MonitoringNamespace:   monNS,
			InfraNamespace:        infraNS,
			Recorder:              recorder,
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

	t.Run("maas-api annotation and Loki Secret switch both datasources", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, string(tenantreconcile.UsageLogsProxyMaaSAPI), true, nil)

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

	t.Run("maas-api annotation without the Loki Secret keeps rendered defaults", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, string(tenantreconcile.UsageLogsProxyMaaSAPI), false, nil)

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

	t.Run("Loki Secret without the annotation keeps rendered defaults", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, "", true, nil)

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

	t.Run("switching back to legacy restores rendered datasource defaults", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, string(tenantreconcile.UsageLogsProxyMaaSAPI), true, nil)
		ctx := context.Background()

		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		cfg := &maasv1alpha1.Config{}
		g.Expect(cl.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, cfg)).To(Succeed())
		cfg.Annotations[tenantreconcile.UsageLogsProxyAnnotation] = string(tenantreconcile.UsageLogsProxyLegacy)
		g.Expect(cl.Update(ctx, cfg)).To(Succeed())
		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

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

	t.Run("invalid annotation fails closed and emits a warning", func(t *testing.T) {
		g := NewWithT(t)
		recorder := events.NewFakeRecorder(1)
		r, cl := newReconciler(t, "invalid", true, recorder)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		multi := getDatasource(t, cl, "usage-logs-multi-tenancy")
		url, found, err := unstructured.NestedString(multi.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(proxyURL))
		g.Expect(recorder.Events).To(Receive(ContainSubstring("InvalidUsageLogsProxy")))
	})
}
