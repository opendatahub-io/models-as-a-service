//nolint:testpackage
package maas

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func lifecycleTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func TestLifecycleReconciler_CreatesConfigWhenMissing(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: "",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var cfg maasv1alpha1.Config
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg)).To(Succeed())
	if cfg.UID == "" {
		base := cfg.DeepCopy()
		cfg.UID = types.UID("test-uid")
		g.Expect(cl.Patch(context.Background(), &cfg, client.MergeFrom(base))).To(Succeed())
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg)).To(Succeed())
	g.Expect(cfg.Name).To(Equal(maasv1alpha1.ConfigInstanceName))
}

func TestLifecycleReconciler_ConfigTerminatingRequeues(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	now := metav1.NewTime(time.Now())
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.ConfigInstanceName,
			UID:               types.UID("cfg-1"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.finalizer"},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: "",
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))
}

func TestLifecycleReconciler_LinksDefaultTenantToConfig(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	const tenantNS = "models-as-a-service"

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("cfg-uid-tenant"),
		},
	}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
		},
	}

	// Build path to observability manifests relative to this test file
	_, currentFile, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	observabilityPath := filepath.Clean(filepath.Join(
		filepath.Dir(currentFile),
		"..", "..", "..", "..",
		"deployment", "components", "observability", "observability", "dashboards",
	))

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, tenant).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: tenantNS,
		ObservabilityManifestsPath:  observabilityPath,
		MonitoringNamespace:         depNS,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}, &updated)).To(Succeed())
	g.Expect(updated.OwnerReferences).ToNot(BeEmpty())
	ref := updated.OwnerReferences[0]
	g.Expect(ref.UID).To(Equal(types.UID("cfg-uid-tenant")))
	g.Expect(ref.Kind).To(Equal(maasv1alpha1.ConfigKind))
	g.Expect(ref.Controller).To(BeNil())
}

func TestLifecycleReconciler_LinksDefaultAITenantToConfig(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.MaaSControllerDeploymentName,
			Namespace: depNS,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("cfg-uid-aitenant"),
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, aitenant).Build()
	r := &LifecycleReconciler{
		Client:            cl,
		Scheme:            s,
		DeploymentName:    tenantreconcile.MaaSControllerDeploymentName,
		DeploymentNS:      depNS,
		AITenantNamespace: tenantreconcile.DefaultAITenantNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: tenantreconcile.MaaSControllerDeploymentName, Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, &updated)).To(Succeed())
	g.Expect(updated.OwnerReferences).ToNot(BeEmpty())
	ref, found := ownerReferenceToConfig(updated.OwnerReferences, types.UID("cfg-uid-aitenant"))
	g.Expect(found).To(BeTrue())
	g.Expect(ref.Controller).To(BeNil())
}

func ownerReferenceToConfig(refs []metav1.OwnerReference, uid types.UID) (metav1.OwnerReference, bool) {
	for _, ref := range refs {
		if ref.APIVersion == maasv1alpha1.GroupVersion.String() &&
			ref.Kind == maasv1alpha1.ConfigKind &&
			ref.UID == uid {
			return ref, true
		}
	}
	return metav1.OwnerReference{}, false
}

func TestLifecycleReconciler_LimitadorServiceMonitorDefaultInterval(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const monitoringNS = "opendatahub"

	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("cfg-uid-limitador"),
		},
		Spec: maasv1alpha1.ConfigSpec{},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
	r := &LifecycleReconciler{
		Client:              cl,
		Scheme:              s,
		MonitoringNamespace: monitoringNS,
	}

	err := r.ensureLimitadorServiceMonitor(context.Background())
	g.Expect(err).NotTo(HaveOccurred())

	sm := &unstructured.Unstructured{}
	sm.SetAPIVersion("monitoring.coreos.com/v1")
	sm.SetKind("ServiceMonitor")
	g.Expect(cl.Get(context.Background(), client.ObjectKey{
		Name:      "limitador-metrics",
		Namespace: monitoringNS,
	}, sm)).To(Succeed())

	spec, found, err := unstructured.NestedMap(sm.Object, "spec")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())

	endpoints, found, err := unstructured.NestedSlice(spec, "endpoints")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(endpoints).To(HaveLen(1))

	endpoint, ok := endpoints[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(endpoint["interval"]).To(Equal("30s"))
}

func TestLifecycleReconciler_LimitadorServiceMonitorCustomInterval(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const monitoringNS = "opendatahub"

	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("cfg-uid-limitador-custom"),
		},
		Spec: maasv1alpha1.ConfigSpec{
			LimitadorScrapeInterval: "1m",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
	r := &LifecycleReconciler{
		Client:              cl,
		Scheme:              s,
		MonitoringNamespace: monitoringNS,
	}

	err := r.ensureLimitadorServiceMonitor(context.Background())
	g.Expect(err).NotTo(HaveOccurred())

	sm := &unstructured.Unstructured{}
	sm.SetAPIVersion("monitoring.coreos.com/v1")
	sm.SetKind("ServiceMonitor")
	g.Expect(cl.Get(context.Background(), client.ObjectKey{
		Name:      "limitador-metrics",
		Namespace: monitoringNS,
	}, sm)).To(Succeed())

	spec, found, err := unstructured.NestedMap(sm.Object, "spec")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())

	endpoints, found, err := unstructured.NestedSlice(spec, "endpoints")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(endpoints).To(HaveLen(1))

	endpoint, ok := endpoints[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(endpoint["interval"]).To(Equal("1m"))
}

func TestEnsureUsageLogsEnvoyFilter(t *testing.T) {
	const gwNS = "openshift-ingress"
	const monitoringNS = "opendatahub"

	t.Run("disabled by default", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:              cl,
			Scheme:              s,
			GatewayNamespace:    gwNS,
			MonitoringNamespace: monitoringNS,
		}

		err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{
			Name: envoyFilterName, Namespace: gwNS,
		}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no EnvoyFilter should exist when usageLogging is disabled")
	})

	t.Run("enabled creates filter", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(true)},
		}

		// Compute absolute path to the EnvoyFilter manifest from this test file's location.
		_, testFile, _, _ := goruntime.Caller(0)
		efManifest := filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs/envoy-otel-access-log.yaml")

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                  cl,
			Scheme:                  s,
			GatewayNamespace:        gwNS,
			MonitoringNamespace:     monitoringNS,
			EnvoyFilterManifestPath: efManifest,
		}

		err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{
			Name: envoyFilterName, Namespace: gwNS,
		}, ef)).To(Succeed(), "EnvoyFilter should exist after enabling usageLogging")
		g.Expect(ef.GetNamespace()).To(Equal(gwNS))

		configPatches, _, _ := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
		g.Expect(configPatches).NotTo(BeEmpty())
		clusterPatch, _ := configPatches[0].(map[string]any)
		endpoints, _, _ := unstructured.NestedSlice(clusterPatch, "patch", "value", "load_assignment", "endpoints")
		g.Expect(endpoints).NotTo(BeEmpty())
		ep0, _ := endpoints[0].(map[string]any)
		lbEndpoints, _, _ := unstructured.NestedSlice(ep0, "lb_endpoints")
		g.Expect(lbEndpoints).NotTo(BeEmpty())
		lbe0, _ := lbEndpoints[0].(map[string]any)
		addr, _, _ := unstructured.NestedString(lbe0, "endpoint", "address", "socket_address", "address")
		g.Expect(addr).To(Equal("usage-logs-collector.opendatahub.svc.cluster.local"),
			"collector address should be patched with MonitoringNamespace")
	})

	t.Run("deletes existing when disabled", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(false)},
		}
		existingEF := &unstructured.Unstructured{}
		existingEF.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		existingEF.SetName(envoyFilterName)
		existingEF.SetNamespace(gwNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, existingEF).Build()
		r := &LifecycleReconciler{
			Client:              cl,
			Scheme:              s,
			GatewayNamespace:    gwNS,
			MonitoringNamespace: monitoringNS,
		}

		err := r.ensureUsageLogsEnvoyFilter(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
		err = cl.Get(context.Background(), client.ObjectKey{
			Name: envoyFilterName, Namespace: gwNS,
		}, ef)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "EnvoyFilter should be deleted when usageLogging is disabled")
	})
}
