//nolint:testpackage
package maas

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

var (
	testTenantGatewayName      = "maas-default-gateway"
	testTenantGatewayNamespace = "openshift-ingress"
)

func tenantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func tenantTestNamespace(name string) client.Object {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func tenantTestUnstructured(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func TestTenantReconcile_DeletionIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         testNS,
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/hold"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: testNS}}
	res, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement("example.com/hold"), "Tenant reconciler does not mutate finalizers on delete")
}

func TestTenantReconcile_NonSingletonNameIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-default-tenant",
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "not-default-tenant", Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "not-default-tenant", Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty(), "non-singleton should not get a finalizer")
}

func TestTenantReconcile_DefaultTenantDoesNotAddCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty(), "default-tenant teardown is Config-driven; no tenant-cleanup finalizer")
}

func TestTenantReconcile_AITenantManagedDefaultAddsCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   testNS,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement(tenantFinalizer))
}

func TestTenantReconcile_DefaultTenantStripsLegacyCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:  testNS,
			Finalizers: []string{"maas.opendatahub.io/tenant-cleanup"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty())
}

func TestTenantReconcile_AITenantManagedAddsCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "ai-tenant-redteam"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "redteam",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:                          cl,
		Scheme:                          s,
		AppNamespace:                    "opendatahub",
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
		GatewayName:                     testTenantGatewayName,
		GatewayNamespace:                testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement("maas.opendatahub.io/tenant-cleanup"))
}

func TestTenantReconcile_AITenantManagedDefaultDeletionCleansPlatformResources(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	ctx := context.Background()
	now := metav1.NewTime(time.Now())

	const tenantNS = "models-as-a-service"
	const appNS = "odh-ai-gateway-infra"
	const gatewayNS = "openshift-ingress"

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         tenantNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   tenantNS,
			},
		},
	}

	resources := []client.Object{
		tenant,
		tenantTestUnstructured(tenantreconcile.GVKDeployment, appNS, tenantreconcile.MaaSAPIDeploymentName("")),
		tenantTestUnstructured(tenantreconcile.GVKService, appNS, tenantreconcile.MaaSAPIServiceName("")),
		tenantTestUnstructured(tenantreconcile.GVKHTTPRoute, appNS, tenantreconcile.MaaSAPIRouteName("")),
		tenantTestUnstructured(tenantreconcile.GVKCronJob, appNS, tenantreconcile.MaaSAPIKeyCleanupCronJobName("")),
		tenantTestUnstructured(tenantreconcile.GVKTokenRateLimitPolicy, gatewayNS, tenantreconcile.GatewayTokenRateLimitDefaultDenyPolicyName("")),
		tenantTestUnstructured(tenantreconcile.GVKDestinationRule, gatewayNS, tenantreconcile.GatewayDestinationRuleName("")),
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(resources...).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     appNS,
		TenantNamespace:  tenantNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: gatewayNS,
	}

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	for _, obj := range resources[1:] {
		err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %s %s/%s to be deleted", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
	}

	var updated maasv1alpha1.MaasTenantConfig
	err = cl.Get(ctx, client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}, &updated)
	if err == nil {
		g.Expect(updated.Finalizers).NotTo(ContainElement(tenantFinalizer))
	} else {
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
}

func TestTenantReconcile_ManagementStateRemovedWaitsForConfigTeardown(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("ct-uid"),
		},
	}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var ctAfter maasv1alpha1.Config
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &ctAfter)).To(Succeed())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())

	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(readyCond.Reason).To(Equal("WaitingForRemovedTeardown"))
}

func TestTenantReconcile_ManagementStateRemoved_ConfigTerminatingPatchesStatus(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.ConfigInstanceName,
			UID:               types.UID("ct-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test/finalizer"},
		},
	}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ConfigTerminating"))
}

func TestTenantReconcile_ManagementStateUnmanagedSetsIdle(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateUnmanaged,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ManagementStateIdle"))
}

func TestTenantReconcile_UnexpectedManagementStateSetsFailedPhase(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: "InvalidState",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("UnexpectedManagementState"))
}

func TestTenantReconcile_ConfigMissingSkipsPlatform(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigMissing"))
}

func TestTenantReconcile_ConfigEmptyUIDPatchesWaitingForConfigUID(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  "",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("WaitingForConfigUID"))
}

func TestTenantReconcile_ConfigTerminatingSkipsPlatform(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.ConfigInstanceName,
			UID:               types.UID("ct-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigTerminating"))
}

func TestTenantReconcile_AppNamespaceUsesConfiguredAppNamespace(t *testing.T) {
	g := NewWithT(t)
	r := &TenantReconciler{AppNamespace: "opendatahub"}
	g.Expect(r.appNamespaceForTenant()).To(Equal("opendatahub"))
}

func TestTenantReconcile_AppNamespaceReturnsRHOAINamespace(t *testing.T) {
	g := NewWithT(t)
	r := &TenantReconciler{AppNamespace: "redhat-ods-applications"}
	g.Expect(r.appNamespaceForTenant()).To(Equal("redhat-ods-applications"))
}

func TestTenantReconcile_NotFoundIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     "models-as-a-service",
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service"},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}
