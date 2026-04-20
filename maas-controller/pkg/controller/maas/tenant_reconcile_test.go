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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func tenantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	utilruntime.Must(gwapiv1.Install(s))
	return s
}

func TestTenantReconcile_DeletionRemovesFinalizerAfterOwnedConfigMapDeleted(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "opendatahub"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.TenantInstanceName,
			Namespace:         testNS,
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
		},
	}
	trueRef := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-owned",
			Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         maasv1alpha1.GroupVersion.String(),
				Kind:               maasv1alpha1.TenantKind,
				Name:               tenant.Name,
				UID:                tenant.UID,
				Controller:         &trueRef,
				BlockOwnerDeletion: &trueRef,
			}},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, cm).
		Build()

	r := &TenantReconciler{
		Client:            cl,
		Scheme:            s,
		OperatorNamespace: testNS,
		AppNamespace:      testNS,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: testNS}}

	res1, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res1.RequeueAfter).To(Equal(finalizeRequeueInterval), "first pass issues child deletes and requeues")

	res2, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res2.RequeueAfter).To(BeNumerically("==", 0))

	var updated maasv1alpha1.Tenant
	err = cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)
	if apierrors.IsNotFound(err) {
		// Fake client may remove the tenant once the finalizer is gone while deletionTimestamp is set.
	} else {
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(updated.Finalizers).NotTo(ContainElement(tenantFinalizer))
	}

	var cms corev1.ConfigMapList
	g.Expect(cl.List(context.Background(), &cms, client.InNamespace("opendatahub"))).To(Succeed())
	g.Expect(cms.Items).To(BeEmpty())
}

func TestTenantReconcile_DeletionRequeuesWhileOwnedChildTerminating(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "opendatahub"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.TenantInstanceName,
			Namespace:         testNS,
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
		},
	}
	trueRef := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-owned",
			Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         maasv1alpha1.GroupVersion.String(),
				Kind:               maasv1alpha1.TenantKind,
				Name:               tenant.Name,
				UID:                tenant.UID,
				Controller:         &trueRef,
				BlockOwnerDeletion: &trueRef,
			}},
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant, cm).
		Build()

	r := &TenantReconciler{
		Client:            cl,
		Scheme:            s,
		OperatorNamespace: testNS,
		AppNamespace:      testNS,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: testNS}}

	res, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(finalizeRequeueInterval))

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement(tenantFinalizer))
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

func TestTenantReconcile_FinalizerAddedOnFirstReconcile(t *testing.T) {
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
	g.Expect(res.Requeue).To(BeTrue(), "should requeue after adding finalizer")

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement(tenantFinalizer))
}

func TestTenantReconcile_ManagementStateRemovedSetsIdleAndRemovesFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
			Finalizers: []string{tenantFinalizer},
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
	g.Expect(updated.Finalizers).NotTo(ContainElement(tenantFinalizer), "finalizer should be removed in Removed state")

	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(readyCond.Reason).To(Equal("ManagementStateIdle"))
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
			Finalizers: []string{tenantFinalizer},
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

func TestTenantReconcile_DeletionIncludesAppNamespace(t *testing.T) {
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
			Finalizers:        []string{tenantFinalizer},
		},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "maas-default-gateway",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.Tenant{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:            cl,
		Scheme:            s,
		OperatorNamespace: "opendatahub",
		AppNamespace:      testNS,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS},
	})
	// Finalization should succeed (no owned resources) and the object is deleted
	// (fake client removes the object once finalizers are cleared on a deleted resource).
	// The reconciler may return NotFound when trying the final status update — that's OK.
	if err != nil {
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected NotFound (object finalized and deleted), got: %v", err)
	}

	var updated maasv1alpha1.Tenant
	err = cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: testNS}, &updated)
	// Object should be gone (finalizer removed → fake client deletes it)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "tenant should be fully deleted after finalization")
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
