//nolint:testpackage
package maas

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func TestLifecycleReconciler_MaasAPILokiUpgradeEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through make test")
	}

	g := NewWithT(t)
	ctx := context.Background()
	cl, scheme := startLifecycleLokiEnvtest(t)

	const monitoringNamespace = "opendatahub"
	const infraNamespace = "redhat-ai-gateway-infra"
	previousUserURL := usageLogsMaaSAPILokiDatasourceURL + "?only_user=true"
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "usage-logs-all", url: usageLogsMaaSAPILokiDatasourceURL},
		{name: "usage-logs-multi-tenancy", url: previousUserURL},
	} {
		previous := persesDatasourceFixture(tc.name)
		g.Expect(unstructured.SetNestedField(previous.Object, tc.url,
			"spec", "config", "plugin", "spec", "proxy", "spec", "url")).To(Succeed())
		g.Expect(unstructured.SetNestedMap(previous.Object, map[string]any{
			"type":     "file",
			"certPath": usageLogsMaaSAPILokiCACertPath,
		}, "spec", "client", "tls", "caCert")).To(Succeed())
		g.Expect(cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(previous),
			client.FieldOwner("maas-controller"))).To(Succeed())
	}

	reconciler := &LifecycleReconciler{
		Client:                cl,
		Scheme:                scheme,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   monitoringNamespace,
		InfraNamespace:        infraNamespace,
	}
	g.Expect(reconciler.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

	for _, tc := range []struct {
		name         string
		url          string
		caConfigName string
	}{
		{
			name:         "usage-logs-all",
			url:          "https://data-science-usage-logs-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application",
			caConfigName: "data-science-usage-logs-lokistack-gateway-ca-bundle",
		},
		{
			name:         "usage-logs-multi-tenancy",
			url:          "https://usage-logs-tenancy-proxy.opendatahub.svc:8443",
			caConfigName: "usage-logs-tenancy-proxy-ca-bundle",
		},
	} {
		datasource := getPersesEnvtest(t, cl, "PersesDatasource", tc.name, monitoringNamespace)
		url, found, err := unstructured.NestedString(datasource.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal(tc.url))

		caType, found, err := unstructured.NestedString(datasource.Object,
			"spec", "client", "tls", "caCert", "type")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caType).To(Equal("configmap"))

		caName, found, err := unstructured.NestedString(datasource.Object,
			"spec", "client", "tls", "caCert", "name")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(caName).To(Equal(tc.caConfigName))

		certPath, found, err := unstructured.NestedString(datasource.Object,
			"spec", "client", "tls", "caCert", "certPath")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(certPath).To(Equal("service-ca.crt"))
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: tenantreconcile.LokiReadSecretName, Namespace: infraNamespace,
	}}
	g.Expect(cl.Delete(ctx, secret)).To(Succeed())
	g.Expect(reconciler.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())

	expectPersesEnvtestNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs", monitoringNamespace)
	expectPersesEnvtestNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs-admin", monitoringNamespace)
	expectPersesEnvtestNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-multi-tenancy", monitoringNamespace)
	expectPersesEnvtestNotFound(g, cl, "PersesDatasource", "maas-portal-usage-logs-all", monitoringNamespace)
}

func TestLifecycleReconciler_PortalCopyReplacementEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through make test")
	}

	g := NewWithT(t)
	ctx := context.Background()
	cl, scheme := startLifecycleLokiEnvtest(t)
	reconciler := &LifecycleReconciler{
		Client:                cl,
		Scheme:                scheme,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
		MonitoringNamespace:   "opendatahub",
		InfraNamespace:        "redhat-ai-gateway-infra",
	}
	g.Expect(reconciler.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())
	getPersesEnvtest(t, cl, "PersesDashboard", "maas-portal-usage-logs", "opendatahub")

	applyClient := &replaceOnApplyClient{
		Client:     cl,
		targetName: "maas-portal-usage-logs-multi-tenancy",
	}
	reconciler.Client = applyClient
	err := reconciler.ensureUsageLogs(ctx, ctrl.Log)
	g.Expect(err).To(HaveOccurred())
	g.Expect(apierrors.IsConflict(err) || apierrors.IsInvalid(err)).To(BeTrue(),
		"stale SSA should be rejected: %v", err)
	g.Expect(applyClient.appliedResourceVersion).To(Equal(applyClient.observedResourceVersion))
	g.Expect(applyClient.appliedResourceVersion).NotTo(BeEmpty())
	g.Expect(applyClient.appliedUID).To(Equal(applyClient.observedUID))

	reconciler.Client = cl
	g.Expect(reconciler.ensureUsageLogs(ctx, ctrl.Log)).To(Succeed())
	expectPersesEnvtestNotFound(g, cl, "PersesDashboard", "maas-portal-usage-logs", "opendatahub")

	datasource := getPersesEnvtest(t, cl, "PersesDatasource",
		"maas-portal-usage-logs-multi-tenancy", "opendatahub")
	g.Expect(datasource.GetOwnerReferences()).To(BeEmpty())
	foreignSpec, found, err := unstructured.NestedBool(datasource.Object, "spec", "foreign")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(foreignSpec).To(BeTrue())
}

func startLifecycleLokiEnvtest(t *testing.T) (client.Client, *k8sruntime.Scheme) {
	t.Helper()
	g := NewWithT(t)
	ctx := context.Background()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	environment := &envtest.Environment{
		Scheme: scheme,
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "..",
				"deployment", "base", "maas-controller", "crd", "bases")},
		},
	}
	config, err := environment.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() {
		_ = environment.Stop()
	})

	_, err = envtest.InstallCRDs(config, envtest.CRDInstallOptions{
		Scheme: scheme,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			persesTestCRD("PersesDatasource", "persesdatasources"),
			persesTestCRD("PersesDashboard", "persesdashboards"),
		},
	})
	g.Expect(err).NotTo(HaveOccurred())

	cl, err := client.New(config, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	for _, namespace := range []string{"opendatahub", "redhat-ai-gateway-infra"} {
		g.Expect(cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
	}
	g.Expect(cl.Create(ctx, &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName},
	})).To(Succeed())
	g.Expect(cl.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.LokiReadSecretName,
			Namespace: "redhat-ai-gateway-infra",
		},
		Data: map[string][]byte{
			tenantreconcile.LokiReadSecretKey: []byte("https://loki.example.svc"),
		},
	})).To(Succeed())

	return cl, scheme
}

type replaceOnApplyClient struct {
	client.Client
	targetName              string
	replaced                bool
	appliedResourceVersion  string
	observedResourceVersion string
	observedUID             types.UID
	appliedUID              types.UID
}

func (c *replaceOnApplyClient) Apply(ctx context.Context, config k8sruntime.ApplyConfiguration, opts ...client.ApplyOption) error {
	obj, isObject := config.(client.Object)
	if !c.replaced && isObject && obj.GetName() == c.targetName {
		c.replaced = true

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		existing.SetName(obj.GetName())
		existing.SetNamespace(obj.GetNamespace())
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
			return err
		}
		c.observedResourceVersion = existing.GetResourceVersion()
		c.observedUID = existing.GetUID()
		c.appliedResourceVersion = obj.GetResourceVersion()
		c.appliedUID = obj.GetUID()

		existing.Object["spec"] = map[string]any{"foreign": true}
		existing.SetOwnerReferences(nil)
		labels := existing.GetLabels()
		delete(labels, "app.kubernetes.io/managed-by")
		existing.SetLabels(labels)
		if err := c.Update(ctx, existing); err != nil {
			return err
		}
	}
	return c.Client.Apply(ctx, config, opts...)
}

func getPersesEnvtest(t *testing.T, cl client.Client, kind, name, namespace string) *unstructured.Unstructured {
	t.Helper()
	g := NewWithT(t)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "perses.dev", Version: "v1alpha1", Kind: kind,
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	return obj
}

func expectPersesEnvtestNotFound(g *WithT, cl client.Client, kind, name, namespace string) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "perses.dev", Version: "v1alpha1", Kind: kind,
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Perses object should be deleted: %v", err)
}

func persesTestCRD(kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".perses.dev"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "perses.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
					},
				},
			}},
		},
	}
}
