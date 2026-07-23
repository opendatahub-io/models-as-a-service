//nolint:testpackage
package maas

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func lifecycleUsageLogsPath(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve lifecycle test file path")
	}
	return filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")
}

func lifecycleTestUnstructured(gvk schema.GroupVersionKind, namespace, name string, finalizers ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if len(finalizers) > 0 {
		obj.SetFinalizers(finalizers)
	}
	return obj
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

	// Compute absolute path to the usage-logs manifest from this test file's location.
	_, testFile, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	usageLogsPath := filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: "",
		UsageLogsManifestPath:       usageLogsPath,
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

func TestLifecycleReconciler_DoesNotRecreateConfigWhenTeardownRequested(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
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

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var cfg maasv1alpha1.Config
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg)).ToNot(Succeed())
}

func TestLifecycleReconciler_TeardownRequestedDeletesConfigAndMarksCompleted(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
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
			UID:  types.UID("cfg-delete-request"),
		},
	}
	aitenantNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":       "maas-controller",
				"opendatahub.io/generated-namespace": "true",
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, aitenantNS).Build()
	r := &LifecycleReconciler{
		Client:            cl,
		Scheme:            s,
		DeploymentName:    "maas-controller",
		DeploymentNS:      depNS,
		AITenantNamespace: tenantreconcile.DefaultAITenantNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Config/default is deleted as a plain step, no finalizer involved.
	var updatedCfg maasv1alpha1.Config
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &updatedCfg))).To(BeTrue())

	// MaaS teardown must preserve every namespace, including the controller-created
	// AITenant infrastructure namespace.
	var survivingNamespace corev1.Namespace
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.DefaultAITenantNamespace}, &survivingNamespace)).To(Succeed())

	// The completion signal must be observable on the Deployment regardless of Config's fate.
	var updatedDep appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updatedDep)).To(Succeed())
	g.Expect(updatedDep.Annotations[TeardownCompletedAnnotation]).To(Equal("true"))
}

func TestLifecycleReconciler_MarkTeardownCompletedIsIdempotent(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			Annotations: map[string]string{
				TeardownCompletedAnnotation: "true",
			},
			ResourceVersion: "1",
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build()
	r := &LifecycleReconciler{Client: cl, Scheme: s}

	g.Expect(r.markTeardownCompleted(context.Background(), dep)).To(Succeed())

	var updated appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updated)).To(Succeed())
	g.Expect(updated.ResourceVersion).To(Equal("1"), "already-annotated Deployment should not be patched again")
}

func TestLifecycleReconciler_TeardownRequestedWithoutConfigRequestsOrphanCleanup(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}
	aitenant := lifecycleTestUnstructured(
		schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "AITenant"},
		tenantreconcile.DefaultAITenantNamespace,
		tenantreconcile.DefaultAITenantName,
		aitenantFinalizer,
	)

	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(dep, aitenant).Build()
	r := &LifecycleReconciler{
		Client:            cl,
		Scheme:            s,
		DeploymentName:    "maas-controller",
		DeploymentNS:      depNS,
		AITenantNamespace: tenantreconcile.DefaultAITenantNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(teardownRequeueAfter))

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(aitenant.GroupVersionKind())
	g.Expect(cl.Get(context.Background(), client.ObjectKey{
		Name:      tenantreconcile.DefaultAITenantName,
		Namespace: tenantreconcile.DefaultAITenantNamespace,
	}, updated)).To(Succeed())
	g.Expect(updated.GetDeletionTimestamp()).NotTo(BeNil())

	// The completion annotation must not be set yet: AITenant deletion is still pending.
	var updatedDep appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updatedDep)).To(Succeed())
	g.Expect(updatedDep.Annotations[TeardownCompletedAnnotation]).To(BeEmpty())
}

func TestLifecycleReconciler_NormalReconcileDoesNotSetDeploymentOwnerReference(t *testing.T) {
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
	cfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("cfg-1"),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: "",
		UsageLogsManifestPath:       lifecycleUsageLogsPath(t),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Config must never own the Deployment: the Deployment carries the teardown
	// annotations and must survive Config being deleted (accidentally, or during
	// teardown), so it cannot be a GC dependent of Config.
	var updated appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updated)).To(Succeed())
	g.Expect(updated.OwnerReferences).To(BeEmpty())
}

func TestLifecycleReconciler_StripsLegacyDeploymentConfigOwnerReferenceOnNormalReconcile(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: maasv1alpha1.GroupVersion.String(),
					Kind:       maasv1alpha1.ConfigKind,
					Name:       maasv1alpha1.ConfigInstanceName,
					UID:        types.UID("cfg-1"),
				},
				{
					APIVersion: "v1",
					Kind:       "Secret",
					Name:       "unrelated",
					UID:        types.UID("secret-1"),
				},
			},
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
			UID:  types.UID("cfg-1"),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg).Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		DeploymentName:        "maas-controller",
		DeploymentNS:          depNS,
		UsageLogsManifestPath: lifecycleUsageLogsPath(t),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updated)).To(Succeed())
	g.Expect(updated.OwnerReferences).To(HaveLen(1), "only the legacy Config ownerReference should be removed")
	g.Expect(updated.OwnerReferences[0].Name).To(Equal("unrelated"))
}

func TestLifecycleReconciler_TeardownStripsLegacyOwnerReferenceBeforeDeletingConfig(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)

	const depNS = "opendatahub"
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: depNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: maasv1alpha1.GroupVersion.String(),
					Kind:       maasv1alpha1.ConfigKind,
					Name:       maasv1alpha1.ConfigInstanceName,
					UID:        types.UID("cfg-legacy"),
				},
			},
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
			UID:  types.UID("cfg-legacy"),
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg).Build()
	r := &LifecycleReconciler{
		Client:         cl,
		Scheme:         s,
		DeploymentName: "maas-controller",
		DeploymentNS:   depNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updatedDep appsv1.Deployment
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-controller", Namespace: depNS}, &updatedDep)).To(Succeed())
	g.Expect(updatedDep.OwnerReferences).To(BeEmpty(), "legacy Config ownerReference must be gone before Config is deleted")
	g.Expect(updatedDep.Annotations[TeardownCompletedAnnotation]).To(Equal("true"))

	var updatedCfg maasv1alpha1.Config
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &updatedCfg))).To(BeTrue())
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

	// Compute absolute path to the usage-logs manifest from this test file's location.
	_, testFile, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	usageLogsPath := filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, tenant).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: tenantNS,
		ObservabilityManifestsPath:  observabilityPath,
		MonitoringNamespace:         depNS,
		UsageLogsManifestPath:       usageLogsPath,
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

	// Compute absolute path to the usage-logs manifest from this test file's location.
	_, testFile, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	usageLogsPath := filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, aitenant).Build()
	r := &LifecycleReconciler{
		Client:                cl,
		Scheme:                s,
		DeploymentName:        tenantreconcile.MaaSControllerDeploymentName,
		DeploymentNS:          depNS,
		AITenantNamespace:     tenantreconcile.DefaultAITenantNamespace,
		UsageLogsManifestPath: usageLogsPath,
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
		g.Expect(addr).To(Equal("usage-logs-collector.opendatahub.svc"),
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

func TestEnsureUsageLogs(t *testing.T) {
	const monitoringNS = "redhat-ods-monitoring"

	gvkOpenTelemetryCollector := schema.GroupVersionKind{
		Group: "opentelemetry.io", Version: "v1beta1", Kind: "OpenTelemetryCollector",
	}
	gvkClusterRoleBinding := schema.GroupVersionKind{
		Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding",
	}

	// Compute absolute path to the usage-logs manifest from this test file's location.
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("failed to get test file path")
	}
	usageLogsPath := filepath.Join(filepath.Dir(testFile), "../../../../deployment/components/observability/usage-logs")

	t.Run("disabled deletes controller-managed resources", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(false)},
		}

		otelCR := &unstructured.Unstructured{}
		otelCR.SetGroupVersionKind(gvkOpenTelemetryCollector)
		otelCR.SetName("usage-logs")
		otelCR.SetNamespace(monitoringNS)
		otelCR.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "maas-controller",
		})

		crb := &unstructured.Unstructured{}
		crb.SetGroupVersionKind(gvkClusterRoleBinding)
		crb.SetName("usage-collector-application-logs-write")
		crb.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        cfg.UID,
			Controller: ptr.To(true),
		}})

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, otelCR, crb).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			MonitoringNamespace:   monitoringNS,
			UsageLogsManifestPath: usageLogsPath,
		}

		err := r.ensureUsageLogs(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkOpenTelemetryCollector)
		err = cl.Get(context.Background(), client.ObjectKey{Name: "usage-logs", Namespace: monitoringNS}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "controller-managed OpenTelemetryCollector should be deleted")

		got.SetGroupVersionKind(gvkClusterRoleBinding)
		err = cl.Get(context.Background(), client.ObjectKey{Name: "usage-collector-application-logs-write"}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "controller-owned ClusterRoleBinding should be deleted")
	})

	t.Run("disabled preserves unowned resources", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(false)},
		}

		// Pre-existing foreign OpenTelemetryCollector with same name but no ownership
		foreignOtelCR := &unstructured.Unstructured{}
		foreignOtelCR.SetGroupVersionKind(gvkOpenTelemetryCollector)
		foreignOtelCR.SetName("usage-logs")
		foreignOtelCR.SetNamespace(monitoringNS)
		// No managed-by label, no OwnerReferences

		// Pre-existing foreign ClusterRoleBinding with same name
		foreignCRB := &unstructured.Unstructured{}
		foreignCRB.SetGroupVersionKind(gvkClusterRoleBinding)
		foreignCRB.SetName("usage-collector-application-logs-write")
		// No ownership metadata

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, foreignOtelCR, foreignCRB).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			MonitoringNamespace:   monitoringNS,
			UsageLogsManifestPath: usageLogsPath,
		}

		err := r.ensureUsageLogs(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkOpenTelemetryCollector)
		err = cl.Get(context.Background(), client.ObjectKey{Name: "usage-logs", Namespace: monitoringNS}, got)
		g.Expect(err).NotTo(HaveOccurred(), "foreign OpenTelemetryCollector should be preserved (CWE-284)")
		g.Expect(got.GetLabels()).NotTo(HaveKey("app.kubernetes.io/managed-by"),
			"foreign resource should not have managed-by label")

		got.SetGroupVersionKind(gvkClusterRoleBinding)
		err = cl.Get(context.Background(), client.ObjectKey{Name: "usage-collector-application-logs-write"}, got)
		g.Expect(err).NotTo(HaveOccurred(), "foreign ClusterRoleBinding should be preserved (CWE-284)")
		g.Expect(got.GetOwnerReferences()).To(BeEmpty(),
			"foreign resource should not have OwnerReferences")
	})

	t.Run("enabled applies resources with monitoring namespace", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(true)},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			MonitoringNamespace:   monitoringNS,
			UsageLogsManifestPath: usageLogsPath,
		}

		err := r.ensureUsageLogs(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkOpenTelemetryCollector)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "usage-logs", Namespace: monitoringNS}, got)).
			To(Succeed(), "OpenTelemetryCollector should exist when usageLogging is enabled")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkClusterRoleBinding)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "usage-collector-application-logs-write"}, got)).
			To(Succeed(), "ClusterRoleBinding should exist when usageLogging is enabled")

		subjects, found, err := unstructured.NestedSlice(got.Object, "subjects")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(subjects).NotTo(BeEmpty())
		subj, ok := subjects[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(subj["namespace"]).To(Equal(monitoringNS))

		dep := &appsv1.Deployment{}
		g.Expect(cl.Get(context.Background(), client.ObjectKey{
			Name: usageLogsTenancyProxyDeploymentName, Namespace: monitoringNS,
		}, dep)).To(Succeed(), "tenancy proxy Deployment should exist when usageLogging is enabled")
	})
}

func TestPatchTenancyProxyImage(t *testing.T) {
	t.Run("patches proxy container image when RELATED_IMAGE set", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv("RELATED_IMAGE_ODH_PYTHON_312_IMAGE", "quay.io/example/tenancy-proxy:test")

		deployment := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name": "usage-logs-tenancy-proxy",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "proxy",
									"image": "registry.redhat.io/ubi9/python-312@sha256:f6713d327d37e654a443752e6654b5aab88f31690e1161eed9c34dd837870172",
								},
							},
						},
					},
				},
			},
		}

		err := patchTenancyProxyImage(deployment)
		g.Expect(err).NotTo(HaveOccurred())

		containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(containers).To(HaveLen(1))

		container, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(container["image"]).To(Equal("quay.io/example/tenancy-proxy:test"))
	})

	t.Run("uses default image when RELATED_IMAGE not set", func(t *testing.T) {
		g := NewWithT(t)

		deployment := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name": "usage-logs-tenancy-proxy",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name": "proxy",
								},
							},
						},
					},
				},
			},
		}

		err := patchTenancyProxyImage(deployment)
		g.Expect(err).NotTo(HaveOccurred())

		containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())

		container, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(container["image"]).To(Equal(DefaultUsageLogsTenancyProxyImage))
	})

	t.Run("ignores non-matching deployment", func(t *testing.T) {
		g := NewWithT(t)
		t.Setenv("RELATED_IMAGE_ODH_PYTHON_312_IMAGE", "quay.io/example/tenancy-proxy:test")

		deployment := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name": "some-other-deployment",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name":  "proxy",
									"image": "registry.redhat.io/ubi9/python-312@sha256:f6713d327d37e654a443752e6654b5aab88f31690e1161eed9c34dd837870172",
								},
							},
						},
					},
				},
			},
		}

		err := patchTenancyProxyImage(deployment)
		g.Expect(err).NotTo(HaveOccurred())

		containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())

		container, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(container["image"]).To(Equal("registry.redhat.io/ubi9/python-312@sha256:f6713d327d37e654a443752e6654b5aab88f31690e1161eed9c34dd837870172"))
	})
}

func TestPatchPersesDatasourceURL(t *testing.T) {
	t.Run("expands short service reference to FQDN", func(t *testing.T) {
		g := NewWithT(t)

		datasource := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "perses.dev/v1alpha1",
				"kind":       "PersesDatasource",
				"metadata": map[string]any{
					"name":      "usage-logs",
					"namespace": "redhat-ods-monitoring",
				},
				"spec": map[string]any{
					"config": map[string]any{
						"plugin": map[string]any{
							"spec": map[string]any{
								"proxy": map[string]any{
									"spec": map[string]any{
										"url": "https://usage-logs-tenancy-proxy:8443",
									},
								},
							},
						},
					},
				},
			},
		}

		err := patchPersesDatasourceURL(datasource)
		g.Expect(err).NotTo(HaveOccurred())

		url, found, err := unstructured.NestedString(datasource.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://usage-logs-tenancy-proxy.redhat-ods-monitoring.svc:8443"))
	})

	t.Run("expands service reference with path", func(t *testing.T) {
		g := NewWithT(t)

		datasource := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "perses.dev/v1alpha1",
				"kind":       "PersesDatasource",
				"metadata": map[string]any{
					"name":      "usage-logs-admin",
					"namespace": "opendatahub",
				},
				"spec": map[string]any{
					"config": map[string]any{
						"plugin": map[string]any{
							"spec": map[string]any{
								"proxy": map[string]any{
									"spec": map[string]any{
										"url": "https://usage-gateway-http:8080/api/logs/v1/application",
									},
								},
							},
						},
					},
				},
			},
		}

		err := patchPersesDatasourceURL(datasource)
		g.Expect(err).NotTo(HaveOccurred())

		url, found, err := unstructured.NestedString(datasource.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://usage-gateway-http.opendatahub.svc:8080/api/logs/v1/application"))
	})

	t.Run("skips already-qualified URLs", func(t *testing.T) {
		g := NewWithT(t)

		datasource := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "perses.dev/v1alpha1",
				"kind":       "PersesDatasource",
				"metadata": map[string]any{
					"name":      "usage-logs",
					"namespace": "opendatahub",
				},
				"spec": map[string]any{
					"config": map[string]any{
						"plugin": map[string]any{
							"spec": map[string]any{
								"proxy": map[string]any{
									"spec": map[string]any{
										"url": "https://usage-logs-tenancy-proxy.opendatahub.svc:8443",
									},
								},
							},
						},
					},
				},
			},
		}

		err := patchPersesDatasourceURL(datasource)
		g.Expect(err).NotTo(HaveOccurred())

		url, found, err := unstructured.NestedString(datasource.Object,
			"spec", "config", "plugin", "spec", "proxy", "spec", "url")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(url).To(Equal("https://usage-logs-tenancy-proxy.opendatahub.svc:8443"),
			"should not modify already-qualified URL")
	})

	t.Run("ignores non-PersesDatasource resources", func(t *testing.T) {
		g := NewWithT(t)

		configMap := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "opendatahub",
				},
			},
		}

		err := patchPersesDatasourceURL(configMap)
		g.Expect(err).NotTo(HaveOccurred())
	})
}
