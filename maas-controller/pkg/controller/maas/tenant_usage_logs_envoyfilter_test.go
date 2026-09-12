//nolint:testpackage
package maas

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

const (
	usageLogsTestGatewayNS     = "openshift-ingress"
	usageLogsTestMonitoringNS  = "opendatahub"
	usageLogsTestAITenantNS    = tenantreconcile.DefaultAITenantNamespace
	usageLogsTestDefaultTenant = "models-as-a-service"
)

func testUsageLogsManifestPath(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file location")
	}
	return filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")
}

func usageLogsTenantConfig(namespace string, tenantName string, aitenantNS string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: namespace,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantName,
				tenantreconcile.LabelAIGatewayTenant:   tenantName,
				tenantreconcile.LabelTenantNamespace:   namespace,
				tenantreconcile.LabelGatewayAccess:     "true",
				"app.kubernetes.io/managed-by":         "maas-controller",
				"app.kubernetes.io/part-of":            tenantreconcile.ComponentName,
			},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      tenantName,
				tenantreconcile.AnnotationAITenantNamespace: aitenantNS,
			},
		},
	}
}

func usageLogsConfig(enabled bool) *maasv1alpha1.Config {
	return &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(enabled)},
	}
}

func newUsageLogsReconciler(t *testing.T, s *runtime.Scheme, cl client.Client, mutate func(*TenantReconciler)) *TenantReconciler {
	t.Helper()
	r := &TenantReconciler{
		Client:              cl,
		Scheme:              s,
		GatewayNamespace:    usageLogsTestGatewayNS,
		MonitoringNamespace: usageLogsTestMonitoringNS,
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func defaultUsageLogsPlatformContext() tenantreconcile.PlatformContext {
	return tenantreconcile.PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Name: tenantreconcile.DefaultAITenantName, Namespace: usageLogsTestGatewayNS},
	}
}

func TestTenantEnsureUsageLogsEnvoyFilter(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(false)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, nil)

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	t.Run("monitoring namespace empty skips without warning", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.MonitoringNamespace = ""
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())
	})

	t.Run("deletes existing when disabled and monitoring namespace empty", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(false)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)
		existingEF := &unstructured.Unstructured{}
		existingEF.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		existingEF.SetName(tenantreconcile.UsageLogsEnvoyFilterName(""))
		existingEF.SetNamespace(usageLogsTestGatewayNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant, existingEF).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.MonitoringNamespace = ""
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	t.Run("monitoring namespace empty retains existing filter", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)
		existingEF := &unstructured.Unstructured{}
		existingEF.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		existingEF.SetName(tenantreconcile.UsageLogsEnvoyFilterName(""))
		existingEF.SetNamespace(usageLogsTestGatewayNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant, existingEF).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.MonitoringNamespace = ""
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, existingEF)).
			To(Succeed())
	})

	t.Run("enabled creates per-tenant filter for default tenant", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.UsageLogsManifestPath = testUsageLogsManifestPath(t)
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)).
			To(Succeed())

		wsLabels, _, _ := unstructured.NestedStringMap(ef.Object, "spec", "workloadSelector", "labels")
		g.Expect(wsLabels["gateway.networking.k8s.io/gateway-name"]).To(Equal(tenantreconcile.DefaultAITenantName))

		_, targetRefsFound, _ := unstructured.NestedSlice(ef.Object, "spec", "targetRefs")
		g.Expect(targetRefsFound).To(BeFalse())

		configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(configPatches).NotTo(BeEmpty())
		patch0, ok := configPatches[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		endpoints, found, err := unstructured.NestedSlice(patch0, "patch", "value", "load_assignment", "endpoints")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(endpoints).NotTo(BeEmpty())
		ep0, ok := endpoints[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		lbEndpoints, found, err := unstructured.NestedSlice(ep0, "lb_endpoints")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(lbEndpoints).NotTo(BeEmpty())
		lbe0, ok := lbEndpoints[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		addr, found, err := unstructured.NestedString(lbe0, "endpoint", "address", "socket_address", "address")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(addr).To(Equal("usage-logs-collector.opendatahub.svc"))

		g.Expect(ef.GetLabels()).To(HaveKeyWithValue(tenantreconcile.LabelManagedByAITenant, "true"))
		g.Expect(ef.GetLabels()).To(HaveKeyWithValue(tenantreconcile.LabelTenantName, tenantreconcile.DefaultAITenantName))
		g.Expect(ef.GetAnnotations()).To(HaveKeyWithValue(tenantreconcile.AnnotationAITenantName, tenantreconcile.DefaultAITenantName))
		g.Expect(ef.GetAnnotations()).To(HaveKeyWithValue(tenantreconcile.AnnotationAITenantNamespace, usageLogsTestAITenantNS))
	})

	t.Run("enabled creates per-tenant filter for named tenant", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig("ai-tenant-redteam", "redteam", usageLogsTestAITenantNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.UsageLogsManifestPath = testUsageLogsManifestPath(t)
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, tenantreconcile.PlatformContext{
			GatewayRef: maasv1alpha1.TenantGatewayRef{Name: "redteam", Namespace: usageLogsTestGatewayNS},
		}, cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName("redteam"), Namespace: usageLogsTestGatewayNS}, ef)).
			To(Succeed())
	})

	t.Run("deletes existing when disabled", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(false)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)
		existingEF := &unstructured.Unstructured{}
		existingEF.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		existingEF.SetName(tenantreconcile.UsageLogsEnvoyFilterName(""))
		existingEF.SetNamespace(usageLogsTestGatewayNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant, existingEF).Build()
		r := newUsageLogsReconciler(t, s, cl, nil)

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(BeEmpty())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	t.Run("returns warning when manifest not found", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.UsageLogsManifestPath = "/nonexistent"
		})

		warning, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warning).To(Equal("Usage-logs EnvoyFilter not deployed: manifest or CRD not available"))

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
}

func TestTenantEnsureUsageLogsEnvoyFilter_CaptureUser(t *testing.T) {
	findUserIDAttr := func(ef *unstructured.Unstructured) bool {
		patches, _, _ := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
		for _, p := range patches {
			patch, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if patch["applyTo"] != "NETWORK_FILTER" {
				continue
			}
			patchVal, _ := patch["patch"].(map[string]any)
			value, _ := patchVal["value"].(map[string]any)
			typedConfig, _ := value["typed_config"].(map[string]any)
			accessLogs, _ := typedConfig["access_log"].([]any)
			for _, al := range accessLogs {
				alMap, _ := al.(map[string]any)
				alTC, _ := alMap["typed_config"].(map[string]any)
				attrs, _ := alTC["attributes"].(map[string]any)
				values, _ := attrs["values"].([]any)
				for _, v := range values {
					entry, _ := v.(map[string]any)
					if entry["key"] == "user_id" {
						return true
					}
				}
			}
		}
		return false
	}

	t.Run("captureUser false omits user_id from filter", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)
		// Telemetry nil → captureUser defaults to false

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.UsageLogsManifestPath = testUsageLogsManifestPath(t)
		})

		_, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)).To(Succeed())
		g.Expect(findUserIDAttr(ef)).To(BeFalse(), "user_id attribute should be absent when captureUser is false")
	})

	t.Run("captureUser true includes user_id in filter", func(t *testing.T) {
		g := NewWithT(t)
		s := tenantTestScheme(t)

		cfg := usageLogsConfig(true)
		tenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, usageLogsTestAITenantNS)
		tenant.Spec.Telemetry = &maasv1alpha1.TenantTelemetryConfig{
			Metrics: &maasv1alpha1.TenantMetricsConfig{
				CaptureUser: ptr.To(true),
			},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
		r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) {
			r.UsageLogsManifestPath = testUsageLogsManifestPath(t)
		})

		_, err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log, tenant, defaultUsageLogsPlatformContext(), cfg)
		g.Expect(err).NotTo(HaveOccurred())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.UsageLogsEnvoyFilterName(""), Namespace: usageLogsTestGatewayNS}, ef)).To(Succeed())
		g.Expect(findUserIDAttr(ef)).To(BeTrue(), "user_id attribute should be present when captureUser is true")
	})
}

func TestTenantCleanup_RemovesUsageLogsEnvoyFilter(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	ctx := context.Background()

	tenant := usageLogsTenantConfig("ai-tenant-team-ef-delete", "team-ef-delete", tenantreconcile.DefaultAITenantNamespace)
	existingEF := &unstructured.Unstructured{}
	existingEF.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	existingEF.SetName(tenantreconcile.UsageLogsEnvoyFilterName("team-ef-delete"))
	existingEF.SetNamespace(usageLogsTestGatewayNS)

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(tenant, existingEF).Build()
	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     "opendatahub",
		GatewayNamespace: usageLogsTestGatewayNS,
	}

	g.Expect(r.cleanupTenantResources(ctx, ctrl.Log, tenant)).To(Succeed())

	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	err := cl.Get(ctx, client.ObjectKey{
		Name:      tenantreconcile.UsageLogsEnvoyFilterName("team-ef-delete"),
		Namespace: usageLogsTestGatewayNS,
	}, ef)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestTenantReconcile_ConfigChangeEnqueuesAllMaasTenantConfigs(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	ctx := context.Background()

	cfg := usageLogsConfig(true)
	defaultTenant := usageLogsTenantConfig(usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, tenantreconcile.DefaultAITenantNamespace)
	otherTenant := usageLogsTenantConfig("ai-tenant-beta", "beta", tenantreconcile.DefaultAITenantNamespace)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cfg, defaultTenant, otherTenant).
		Build()
	r := &TenantReconciler{
		Client:                          cl,
		Scheme:                          s,
		TenantNamespace:                 usageLogsTestDefaultTenant,
		TenantNamespaceDiscoveryEnabled: true,
	}

	requests := r.mapConfigToMaasTenantConfigs(ctx, cfg)
	g.Expect(requests).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: usageLogsTestDefaultTenant}},
		reconcile.Request{NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "ai-tenant-beta"}},
	))
}
