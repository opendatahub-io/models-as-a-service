//nolint:testpackage
package maas

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

func TestLifecycleReconciler_LokiReadSecretEvents(t *testing.T) {
	const infraNS = "redhat-ai-gateway-infra"
	for _, tc := range []struct {
		name      string
		infraNS   string
		secretNS  string
		secret    string
		wantQueue bool
	}{
		{name: "Loki configuration", infraNS: infraNS, secretNS: infraNS, secret: tenantreconcile.LokiReadSecretName, wantQueue: true},
		{name: "other namespace", infraNS: infraNS, secretNS: "other-infra", secret: tenantreconcile.LokiReadSecretName},
		{name: "other Secret", infraNS: infraNS, secretNS: infraNS, secret: tenantreconcile.MaaSDBSecretName},
		{name: "infrastructure namespace unset", secretNS: infraNS, secret: tenantreconcile.LokiReadSecretName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &LifecycleReconciler{DeploymentName: "maas-controller", DeploymentNS: "controller-ns", InfraNamespace: tc.infraNS}
			h := handler.EnqueueRequestsFromMapFunc(r.enqueueLokiReadSecret)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tc.secret, Namespace: tc.secretNS}}
			updated := secret.DeepCopy()
			updated.ResourceVersion = "2"
			updated.Data = map[string][]byte{tenantreconcile.LokiReadSecretKey: []byte("https://loki.example.svc")}
			for _, ev := range []struct {
				name string
				send func(context.Context, workqueue.TypedRateLimitingInterface[reconcile.Request])
			}{
				{name: "create", send: func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					h.Create(ctx, event.CreateEvent{Object: secret}, queue)
				}},
				{name: "update", send: func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					h.Update(ctx, event.UpdateEvent{ObjectOld: secret, ObjectNew: updated}, queue)
				}},
				{name: "delete", send: func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
					h.Delete(ctx, event.DeleteEvent{Object: secret}, queue)
				}},
			} {
				t.Run(ev.name, func(t *testing.T) {
					g := NewWithT(t)
					queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
					defer queue.ShutDown()
					ev.send(context.Background(), queue)
					if !tc.wantQueue {
						g.Expect(queue.Len()).To(BeZero())
						return
					}
					g.Expect(queue.Len()).To(Equal(1))
					request, shutdown := queue.Get()
					g.Expect(shutdown).To(BeFalse())
					g.Expect(request.NamespacedName).To(Equal(types.NamespacedName{Namespace: r.DeploymentNS, Name: r.DeploymentName}))
					queue.Done(request)
				})
			}
		})
	}
}

func TestLifecycleReconciler_UsageLogsPortalDatasourceWatchPredicate(t *testing.T) {
	r := &LifecycleReconciler{MonitoringNamespace: "opendatahub"}

	for _, tc := range []struct {
		name      string
		namespace string
		objName   string
		want      bool
	}{
		{name: "multi-tenancy copy", namespace: "opendatahub", objName: "maas-portal-usage-logs-multi-tenancy", want: true},
		{name: "all copy", namespace: "opendatahub", objName: "maas-portal-usage-logs-all", want: true},
		{name: "unrelated datasource", namespace: "opendatahub", objName: "usage-logs-multi-tenancy"},
		{name: "dashboard name", namespace: "opendatahub", objName: "maas-portal-usage-logs"},
		{name: "other namespace", namespace: "other", objName: "maas-portal-usage-logs-all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			datasource := &unstructured.Unstructured{}
			datasource.SetNamespace(tc.namespace)
			datasource.SetName(tc.objName)
			g.Expect(r.isUsageLogsPortalDatasource(datasource)).To(Equal(tc.want))
		})
	}
}

func TestLifecycleReconciler_UsageLogsPortalDashboardWatchPredicate(t *testing.T) {
	r := &LifecycleReconciler{MonitoringNamespace: "opendatahub"}

	for _, tc := range []struct {
		name      string
		namespace string
		objName   string
		want      bool
	}{
		{name: "user dashboard", namespace: "opendatahub", objName: "maas-portal-usage-logs", want: true},
		{name: "admin dashboard", namespace: "opendatahub", objName: "maas-portal-usage-logs-admin", want: true},
		{name: "shared dashboard", namespace: "opendatahub", objName: "dashboard-5-maas-usage-logs"},
		{name: "datasource name", namespace: "opendatahub", objName: "maas-portal-usage-logs-all"},
		{name: "other namespace", namespace: "other", objName: "maas-portal-usage-logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			dashboard := &unstructured.Unstructured{}
			dashboard.SetNamespace(tc.namespace)
			dashboard.SetName(tc.objName)
			g.Expect(r.isUsageLogsPortalDashboardWatch(dashboard)).To(Equal(tc.want))
		})
	}
}

func TestPatchPersesDatasourceMaaSAPILoki(t *testing.T) {
	g := NewWithT(t)

	t.Run("Portal admin datasource gets maas-api URL without only_user", func(t *testing.T) {
		ds := persesDatasourceFixture("maas-portal-usage-logs-all")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"))
	})

	t.Run("Portal user datasource gets maas-api URL with only_user=true", func(t *testing.T) {
		ds := persesDatasourceFixture("maas-portal-usage-logs-multi-tenancy")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds)).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs?only_user=true"))
	})

	t.Run("caCert switches to service-ca file and configmap fields are dropped", func(t *testing.T) {
		ds := persesDatasourceFixture("maas-portal-usage-logs-all")
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
		ds := persesDatasourceFixture("maas-portal-usage-logs-multi-tenancy")
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

func TestLifecycleReconciler_MaasAPILokiPortalCopies(t *testing.T) {
	const (
		infraNS  = "redhat-ai-gateway-infra"
		monNS    = "opendatahub"
		proxyURL = "https://usage-logs-tenancy-proxy.opendatahub.svc:8443"
		adminURL = "https://data-science-usage-logs-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"
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
					tenantreconcile.LokiReadSecretKey: []byte("https://data-science-usage-logs-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"),
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

	getPerses := func(t *testing.T, cl client.Client, kind, name string) *unstructured.Unstructured {
		t.Helper()
		g := NewWithT(t)
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(persesGV.WithKind(kind))
		obj.SetName(name)
		obj.SetNamespace(monNS)
		g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		return obj
	}

	getDatasource := func(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
		t.Helper()
		return getPerses(t, cl, "PersesDatasource", name)
	}

	nestedNames := func(value any) map[string]struct{} {
		names := map[string]struct{}{}
		var collect func(any)
		collect = func(node any) {
			switch typed := node.(type) {
			case map[string]any:
				for key, child := range typed {
					if key == "name" {
						if name, ok := child.(string); ok {
							names[name] = struct{}{}
						}
					}
					collect(child)
				}
			case []any:
				for _, child := range typed {
					collect(child)
				}
			}
		}
		collect(value)
		return names
	}

	expectPortalCopyNotFound := func(g *WithT, cl client.Client, kind, name string) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(persesGV.WithKind(kind))
		obj.SetName(name)
		obj.SetNamespace(monNS)
		err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Portal copy %s %s should be deleted: %v", kind, name, err)
	}

	t.Run("secret present creates Portal copies and keeps originals legacy", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, true)

		g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

		admin := getDatasource(t, cl, "usage-logs-all")
		url, found, err := unstructured.NestedString(admin.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(adminURL))

		multi := getDatasource(t, cl, "usage-logs-multi-tenancy")
		url, found, err = unstructured.NestedString(multi.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(proxyURL))

		portalAdmin := getDatasource(t, cl, "maas-portal-usage-logs-all")
		url, found, err = unstructured.NestedString(portalAdmin.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(usageLogsMaaSAPILokiDatasourceURL))
		secret, found, err := unstructured.NestedString(portalAdmin.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "secret")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(secret).To(Equal("maas-portal-usage-logs-all-secret"))

		portalMulti := getDatasource(t, cl, "maas-portal-usage-logs-multi-tenancy")
		url, found, err = unstructured.NestedString(portalMulti.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(usageLogsMaaSAPILokiDatasourceURL + "?only_user=true"))
		secret, found, err = unstructured.NestedString(portalMulti.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "secret")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(secret).To(Equal("maas-portal-usage-logs-multi-tenancy-secret"))

		portalDashboard := getPerses(t, cl, "PersesDashboard", "maas-portal-usage-logs")
		names := nestedNames(portalDashboard.Object)
		g.Expect(names).To(HaveKey("maas-portal-usage-logs-multi-tenancy"))
		g.Expect(names).NotTo(HaveKey("usage-logs-multi-tenancy"))

		portalAdminDashboard := getPerses(t, cl, "PersesDashboard", "maas-portal-usage-logs-admin")
		names = nestedNames(portalAdminDashboard.Object)
		g.Expect(names).To(HaveKey("maas-portal-usage-logs-all"))
		g.Expect(names).NotTo(HaveKey("usage-logs-all"))

		sharedDashboard := getPerses(t, cl, "PersesDashboard", "dashboard-5-maas-usage-logs")
		names = nestedNames(sharedDashboard.Object)
		g.Expect(names).To(HaveKey("usage-logs-multi-tenancy"))
		g.Expect(names).NotTo(HaveKey("maas-portal-usage-logs-multi-tenancy"))
	})

	t.Run("secret absent keeps rendered defaults and no Portal copies", func(t *testing.T) {
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

		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs")
		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs-admin")
		expectPortalCopyNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-multi-tenancy")
		expectPortalCopyNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-all")
	})

	t.Run("upgrade restores shared datasources and removes all Portal copies", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, true)
		ctx := context.Background()

		for _, name := range []string{"usage-logs-all", "usage-logs-multi-tenancy"} {
			previous := persesDatasourceFixture(name)
			g.Expect(unstructured.SetNestedField(previous.Object, usageLogsMaaSAPILokiDatasourceURL,
				"spec", "config", "plugin", "spec", "proxy", "spec", "url")).To(Succeed())
			g.Expect(unstructured.SetNestedMap(previous.Object, map[string]any{
				"type":     "file",
				"certPath": usageLogsMaaSAPILokiCACertPath,
			}, "spec", "client", "tls", "caCert")).To(Succeed())
			g.Expect(cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(previous),
				client.FieldOwner("maas-controller"))).To(Succeed())
		}

		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		for _, tc := range []struct {
			name     string
			expected string
		}{
			{name: "usage-logs-all", expected: adminURL},
			{name: "usage-logs-multi-tenancy", expected: proxyURL},
		} {
			ds := getDatasource(t, cl, tc.name)
			url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(url).To(Equal(tc.expected))

			caType, found, err := unstructured.NestedString(ds.Object, "spec", "client", "tls", "caCert", "type")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(caType).To(Equal("configmap"))
		}

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.LokiReadSecretName, Namespace: infraNS}}
		g.Expect(cl.Delete(ctx, secret)).To(Succeed())
		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs")
		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs-admin")
		expectPortalCopyNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-multi-tenancy")
		expectPortalCopyNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-all")
	})

	t.Run("foreign Portal copies are not adopted or deleted", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, true)
		ctx := context.Background()

		foreign := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "perses.dev/v1alpha1",
			"kind":       "PersesDatasource",
			"metadata": map[string]any{
				"name":      "maas-portal-usage-logs-multi-tenancy",
				"namespace": monNS,
			},
			"spec": map[string]any{"foreign": true},
		}}
		g.Expect(cl.Create(ctx, foreign)).To(Succeed())
		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		stored := getDatasource(t, cl, foreign.GetName())
		g.Expect(stored.GetOwnerReferences()).To(BeEmpty())
		foreignSpec, found, err := unstructured.NestedBool(stored.Object, "spec", "foreign")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(foreignSpec).To(BeTrue())
		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs")

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.LokiReadSecretName, Namespace: infraNS}}
		g.Expect(cl.Delete(ctx, secret)).To(Succeed())
		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		stored = getDatasource(t, cl, foreign.GetName())
		g.Expect(stored.GetOwnerReferences()).To(BeEmpty())
		foreignSpec, found, err = unstructured.NestedBool(stored.Object, "spec", "foreign")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(foreignSpec).To(BeTrue())
		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs")
	})

	t.Run("foreign datasource replacement withdraws its dashboard", func(t *testing.T) {
		g := NewWithT(t)
		r, cl := newReconciler(t, true)
		ctx := context.Background()

		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())
		getPerses(t, cl, "PersesDashboard", "maas-portal-usage-logs")

		ownedDatasource := getDatasource(t, cl, "maas-portal-usage-logs-multi-tenancy")
		g.Expect(cl.Delete(ctx, ownedDatasource)).To(Succeed())
		foreign := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"foreign": true}}}
		foreign.SetGroupVersionKind(persesGV.WithKind("PersesDatasource"))
		foreign.SetName(ownedDatasource.GetName())
		foreign.SetNamespace(ownedDatasource.GetNamespace())
		g.Expect(cl.Create(ctx, foreign)).To(Succeed())

		g.Expect(r.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

		stored := getDatasource(t, cl, foreign.GetName())
		g.Expect(stored.GetOwnerReferences()).To(BeEmpty())
		foreignSpec, found, err := unstructured.NestedBool(stored.Object, "spec", "foreign")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(foreignSpec).To(BeTrue())
		expectPortalCopyNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs")
	})
}
