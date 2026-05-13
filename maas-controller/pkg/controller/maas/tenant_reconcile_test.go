//nolint:testpackage
package maas

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func tenantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func TestTenantReconcile_DeletionIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.TenantInstanceName,
			Namespace:         testNS,
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/hold"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: testNS}}
	res, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement("example.com/hold"), "Tenant reconciler does not mutate finalizers on delete")
}

func TestTenantReconcile_NonSingletonNameIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-default-tenant",
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "not-default-tenant", Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "not-default-tenant", Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty(), "non-singleton should not get a finalizer")
}

func TestTenantReconcile_ManagedReconcileDoesNotAddFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty())
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
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, ct).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var ctAfter maasv1alpha1.Config
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &ctAfter)).To(Succeed())

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())

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
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, ct).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ConfigTerminating"))
}

func TestTenantReconcile_ManagementStateUnmanagedSetsIdle(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateUnmanaged,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ManagementStateIdle"))
}

func TestTenantReconcile_UnexpectedManagementStateSetsFailedPhase(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: "InvalidState",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("UnexpectedManagementState"))
}

func TestTenantReconcile_ConfigMissingSkipsPlatform(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigMissing"))
}

func TestTenantReconcile_ConfigEmptyUIDPatchesWaitingForConfigUID(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
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
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, ct).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	var updated maasv1alpha1.Tenant
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
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
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
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, ct).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: testNS,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(10 * time.Second))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigTerminating"))
}

func TestTenantReconcile_NotFoundIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	r := &TenantReconciler{
		Client:       cl,
		Scheme:       s,
		AppNamespace: "models-as-a-service",
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: "models-as-a-service"},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}
