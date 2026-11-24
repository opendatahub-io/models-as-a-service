//nolint:testpackage
package maas

import (
	"context"
	"errors"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
									"url": "https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs",
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

	t.Run("usage-logs-all gets the infrastructure-qualified maas-api URL", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-all")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds, "redhat-ai-gateway-infra")).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"))
	})

	t.Run("usage-logs-multi-tenancy gets maas-api URL with only_user=true", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-multi-tenancy")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds, "odh-ai-gateway-infra")).To(Succeed())

		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://maas-api.odh-ai-gateway-infra.svc.cluster.local:8443/v1/logs?only_user=true"))
	})

	t.Run("caCert switches to the pod service-ca file", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-all")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds, "redhat-ai-gateway-infra")).To(Succeed())

		caCert, found, err := unstructured.NestedMap(ds.Object, "spec", "client", "tls", "caCert")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caCert).To(Equal(map[string]any{
			"type":     "file",
			"certPath": "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt",
		}))
	})

	t.Run("other datasources and kinds are untouched", func(t *testing.T) {
		ds := persesDatasourceFixture("some-other-datasource")
		g.Expect(patchPersesDatasourceMaaSAPILoki(ds, "redhat-ai-gateway-infra")).To(Succeed())
		url, _, _ := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(url).To(Equal("https://maas-api.redhat-ai-gateway-infra.svc.cluster.local:8443/v1/logs"))

		dep := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"}}
		g.Expect(patchPersesDatasourceMaaSAPILoki(dep, "redhat-ai-gateway-infra")).To(Succeed())
	})

	t.Run("missing infrastructure namespace fails closed", func(t *testing.T) {
		ds := persesDatasourceFixture("usage-logs-all")
		err := patchPersesDatasourceMaaSAPILoki(ds, "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("infrastructure namespace is required"))
	})
}

func TestMaaSAPILokiRBACIncludesApplicationLogsRead(t *testing.T) {
	_, testFile, _, ok := goruntime.Caller(0)
	require.True(t, ok, "resolve Loki RBAC test file path")
	rbacPath := filepath.Join(filepath.Dir(testFile), "../../../../deployment/base/maas-api/rbac")

	resources, err := tenantreconcile.RenderKustomize(rbacPath, "redhat-ai-gateway-infra")
	require.NoError(t, err)

	for _, roleName := range []string{"maas-api", "maas-api-supplemental"} {
		var role *unstructured.Unstructured
		for i := range resources {
			if resources[i].GetKind() == "ClusterRole" && resources[i].GetName() == roleName {
				role = &resources[i]
				break
			}
		}
		require.NotNil(t, role, "ClusterRole %s should be rendered", roleName)

		rules, found, err := unstructured.NestedSlice(role.Object, "rules")
		require.NoError(t, err)
		require.True(t, found, "ClusterRole %s should have rules", roleName)
		require.True(t, hasApplicationLogsReadRule(rules), "ClusterRole %s should grant least-privilege Loki application log reads", roleName)
		require.True(t, hasOnlyApplicationLogsReadRule(rules), "ClusterRole %s should not grant excess Loki permissions", roleName)
	}
}

func hasOnlyApplicationLogsReadRule(rules []any) bool {
	lokiRuleCount := 0
	for _, ruleValue := range rules {
		rule, ok := ruleValue.(map[string]any)
		if !ok {
			continue
		}
		apiGroups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
		if !slices.Contains(apiGroups, "loki.grafana.com") && !slices.Contains(apiGroups, "*") {
			continue
		}

		lokiRuleCount++
		if lokiRuleCount > 1 {
			return false
		}
		resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
		resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
		verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
		if !slices.Equal(apiGroups, []string{"loki.grafana.com"}) ||
			!slices.Equal(resources, []string{"application"}) ||
			!slices.Equal(resourceNames, []string{"logs"}) ||
			!slices.Equal(verbs, []string{"get"}) {
			return false
		}
	}
	return lokiRuleCount == 1
}

func TestHasOnlyApplicationLogsReadRuleRejectsWildcardExcessGrant(t *testing.T) {
	rules := []any{
		map[string]any{
			"apiGroups":     []any{"loki.grafana.com"},
			"resources":     []any{"application"},
			"resourceNames": []any{"logs"},
			"verbs":         []any{"get"},
		},
		map[string]any{
			"apiGroups":     []any{"*"},
			"resources":     []any{"application"},
			"resourceNames": []any{"logs"},
			"verbs":         []any{"delete"},
		},
	}

	require.False(t, hasOnlyApplicationLogsReadRule(rules))
}

func hasApplicationLogsReadRule(rules []any) bool {
	for _, ruleValue := range rules {
		rule, ok := ruleValue.(map[string]any)
		if !ok {
			continue
		}
		apiGroups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
		resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
		verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
		if slices.Contains(apiGroups, "loki.grafana.com") &&
			slices.Contains(resources, "application") &&
			slices.Contains(resourceNames, "logs") &&
			slices.Contains(verbs, "get") {
			return true
		}
	}
	return false
}

func TestLifecycleReconciler_MaasAPILokiDatasource(t *testing.T) {
	const (
		infraNS = "redhat-ai-gateway-infra"
		monNS   = "opendatahub"
	)

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}

	persesGV := schema.GroupVersion{Group: "perses.dev", Version: "v1alpha1"}
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{persesGV})
	mapper.Add(persesGV.WithKind("PersesDatasource"), apimeta.RESTScopeNamespace)
	mapper.Add(persesGV.WithKind("PersesDashboard"), apimeta.RESTScopeNamespace)
	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg).
		WithRESTMapper(mapper).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monNS,
		InfraNamespace:        infraNS,
	}

	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

	datasources := map[string]bool{
		"usage-logs-all":           false,
		"usage-logs-multi-tenancy": true,
	}
	for name, onlyUser := range datasources {
		ds := &unstructured.Unstructured{}
		ds.SetGroupVersionKind(persesGV.WithKind("PersesDatasource"))
		ds.SetName(name)
		ds.SetNamespace(monNS)
		g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(ds), ds)).To(Succeed())

		expected := "https://maas-api." + infraNS + ".svc.cluster.local:8443/v1/logs"
		if onlyUser {
			expected += "?only_user=true"
		}
		url, found, err := unstructured.NestedString(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(expected))

		dropHeaders, found, err := unstructured.NestedStringSlice(ds.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "dropHeaders")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(dropHeaders).To(ConsistOf(
			"X-MaaS-Identity",
			"X-MaaS-Username",
			"X-MaaS-Group",
			"X-MaaS-Organization-Id",
		))

		caType, found, err := unstructured.NestedString(ds.Object, "spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("file"))
	}

	np := &unstructured.Unstructured{}
	np.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"})
	np.SetName("usage-logs-lokistack-allow-maas-api")
	np.SetNamespace(monNS)
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(np), np)).To(Succeed())

	ingress, found, err := unstructured.NestedSlice(np.Object, "spec", "ingress")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(ingress).To(HaveLen(1))
	rule, ok := ingress[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	from, ok := rule["from"].([]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(from).To(HaveLen(1))
	peer, ok := from[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	namespaceSelector, ok := peer["namespaceSelector"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	matchLabels, ok := namespaceSelector["matchLabels"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(matchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", infraNS))
}

func TestLifecycleReconciler_RemovesLegacyTenancyProxy(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	legacy := unstructured.Unstructured{}
	legacy.SetAPIVersion("apps/v1")
	legacy.SetKind("Deployment")
	legacy.SetName("usage-logs-tenancy-proxy")
	legacy.SetNamespace(monitoringNS)
	legacy.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "maas-controller"})
	legacyService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyServiceName,
			Namespace: monitoringNS,
			UID:       types.UID("legacy-service-uid"),
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "maas-controller"},
		},
	}
	legacySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyTLSSecretName,
			Namespace: monitoringNS,
			Annotations: map[string]string{
				serviceCAServiceNameAnnotation: usageLogsTenancyProxyServiceName,
				serviceCAServiceUIDAnnotation:  "legacy-service-uid",
				serviceCAComponentAnnotation:   "service-ca",
			},
		},
		Type: corev1.SecretTypeTLS,
	}

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, &legacy, legacyService, legacySecret).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
		InfraNamespace:        "redhat-ai-gateway-infra",
	}

	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(legacy.GroupVersionKind())
	err := cl.Get(context.Background(), client.ObjectKeyFromObject(&legacy), got)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	err = cl.Get(context.Background(), client.ObjectKeyFromObject(legacySecret), &corev1.Secret{})
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestLifecycleReconciler_PreservesUnrelatedTenancyProxyTLSSecret(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyTLSSecretName,
			Namespace: monitoringNS,
			Annotations: map[string]string{
				serviceCAServiceNameAnnotation: "another-service",
				serviceCAServiceUIDAnnotation:  "another-service-uid",
				serviceCAComponentAnnotation:   "service-ca",
			},
		},
		Type: corev1.SecretTypeTLS,
	}

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, secret).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
		InfraNamespace:        "redhat-ai-gateway-infra",
	}

	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})).To(Succeed())
}

func TestLifecycleReconciler_PreservesTenancyProxyTLSSecretWithMismatchedServiceUID(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyServiceName,
			Namespace: monitoringNS,
			UID:       types.UID("actual-service-uid"),
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "maas-controller"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyTLSSecretName,
			Namespace: monitoringNS,
			Annotations: map[string]string{
				serviceCAServiceNameAnnotation: usageLogsTenancyProxyServiceName,
				serviceCAServiceUIDAnnotation:  "claimed-service-uid",
				serviceCAComponentAnnotation:   "service-ca",
			},
		},
		Type: corev1.SecretTypeTLS,
	}

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, service, secret).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
		InfraNamespace:        "redhat-ai-gateway-infra",
	}

	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})).To(Succeed())
}

func TestLifecycleReconciler_PreservesTenancyProxyTLSSecretForUnownedService(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyServiceName,
			Namespace: monitoringNS,
			UID:       types.UID("foreign-service-uid"),
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyTLSSecretName,
			Namespace: monitoringNS,
			Annotations: map[string]string{
				serviceCAServiceNameAnnotation: usageLogsTenancyProxyServiceName,
				serviceCAServiceUIDAnnotation:  "foreign-service-uid",
				serviceCAComponentAnnotation:   "service-ca",
			},
		},
		Type: corev1.SecretTypeTLS,
	}

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, service, secret).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
		InfraNamespace:        "redhat-ai-gateway-infra",
	}

	g.Expect(r.ensureUsageLogs(context.Background(), ctrl.Log)).To(Succeed())
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})).To(Succeed())
}

func TestLifecycleReconciler_PreservesReplacedTenancyProxyTLSSecret(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyServiceName,
			Namespace: monitoringNS,
			UID:       types.UID("legacy-service-uid"),
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "maas-controller"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      usageLogsTenancyProxyTLSSecretName,
			Namespace: monitoringNS,
			UID:       types.UID("legacy-secret-uid"),
			Annotations: map[string]string{
				serviceCAServiceNameAnnotation: usageLogsTenancyProxyServiceName,
				serviceCAServiceUIDAnnotation:  "legacy-service-uid",
				serviceCAComponentAnnotation:   "service-ca",
			},
		},
		Type: corev1.SecretTypeTLS,
	}

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, service, secret).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() != usageLogsTenancyProxyTLSSecretName {
					return c.Delete(ctx, obj, opts...)
				}
				replacement, ok := obj.DeepCopyObject().(*corev1.Secret)
				g.Expect(ok).To(BeTrue())
				replacement.UID = types.UID("replacement-secret-uid")
				replacement.ResourceVersion = ""
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				if err := c.Create(ctx, replacement); err != nil {
					return err
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNS,
		InfraNamespace:        "redhat-ai-gateway-infra",
	}

	err := r.ensureUsageLogs(context.Background(), ctrl.Log)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsConflict(err)).To(BeTrue())
	got := &corev1.Secret{}
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(secret), got)).To(Succeed())
	g.Expect(got.UID).To(Equal(types.UID("replacement-secret-uid")))
}

func TestDeleteOwnedRenderedResourcesPreservesReplacedResource(t *testing.T) {
	const monitoringNS = "opendatahub"

	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	resource := lifecycleTestUnstructured(
		schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		monitoringNS,
		"usage-logs-tenancy-proxy",
	)
	resource.SetUID(types.UID("legacy-resource-uid"))
	resource.SetResourceVersion("1")
	resource.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "maas-controller"})
	deleteAttempted := false

	cl := newSSAFakeClientBuilder(s).
		WithStatusSubresource(&maasv1alpha1.Config{}).
		WithObjects(cfg, resource).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() != resource.GetName() {
					return c.Delete(ctx, obj, opts...)
				}
				deleteAttempted = true
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				g.Expect(options.Preconditions).NotTo(BeNil())
				g.Expect(string(*options.Preconditions.UID)).To(Equal("legacy-resource-uid"))
				g.Expect(*options.Preconditions.ResourceVersion).To(Equal("1"))
				return apierrors.NewConflict(
					schema.GroupResource{Group: "apps", Resource: "deployments"},
					resource.GetName(),
					errors.New("resource was replaced"),
				)
			},
		}).
		Build()
	r := &LifecycleReconciler{Client: cl, Scheme: s}

	err := r.deleteOwnedRenderedResources(
		context.Background(),
		ctrl.Log,
		*cfg,
		[]unstructured.Unstructured{*resource},
		"removed Python usage-logs tenancy proxy",
	)
	g.Expect(deleteAttempted).To(BeTrue())
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsConflict(err)).To(BeTrue())
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(resource), resource.DeepCopy())).To(Succeed())
}
