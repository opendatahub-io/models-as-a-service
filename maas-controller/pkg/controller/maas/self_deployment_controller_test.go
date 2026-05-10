//nolint:testpackage
package maas

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func selfDepScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := tenantTestScheme(t)
	utilruntime.Must(appsv1.AddToScheme(s))
	utilruntime.Must(rbacv1.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	return s
}

func TestLifecycleReconciler(t *testing.T) {
	const (
		depName  = "maas-controller"
		depNS    = "redhat-ods-applications"
		tenantNS = "models-as-a-service"
	)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: depName, Namespace: depNS}}

	t.Run("adds cleanup finalizer when Deployment has none", func(t *testing.T) {
		g := NewWithT(t)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: depNS}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS, TenantNamespace: tenantNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		var updated appsv1.Deployment
		g.Expect(cli.Get(t.Context(), req.NamespacedName, &updated)).To(Succeed())
		g.Expect(updated.Finalizers).To(ContainElement(CleanupFinalizer))
	})

	t.Run("no-op when finalizer already present and Deployment is running", func(t *testing.T) {
		g := NewWithT(t)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: depName, Namespace: depNS,
			Finalizers: []string{CleanupFinalizer},
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS, TenantNamespace: tenantNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))
	})

	t.Run("no-op when Deployment is terminating but CleanupFinalizer is absent", func(t *testing.T) {
		// W4b contract: if the finalizer was never set (e.g. controller first deployed against
		// a Deployment that is immediately deleted), Reconcile returns cleanly with no error
		// and skips all teardown rather than panicking or re-queuing indefinitely.
		//
		// The fake client refuses to store objects with DeletionTimestamp but no finalizers, so we
		// use a placeholder finalizer (not CleanupFinalizer) and Delete to trigger DeletionTimestamp.
		g := NewWithT(t)
		const placeholderFinalizer = "test/placeholder"
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:       depName,
			Namespace:  depNS,
			Finalizers: []string{placeholderFinalizer},
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		g.Expect(cli.Delete(t.Context(), dep)).To(Succeed())

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS, TenantNamespace: tenantNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))
	})

	t.Run("removes finalizer cleanly when no labeled cluster resources exist", func(t *testing.T) {
		// W4c contract: if no ClusterRoles/CRDs/ClusterRoleBindings match the component label
		// (e.g. operator did not stamp the label), deleteClusterScopedResources still returns
		// nil and the finalizer is released — no panic, no error, no requeue.
		g := NewWithT(t)
		now := metav1.NewTime(time.Now())
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: depName, Namespace: depNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{CleanupFinalizer},
		}}
		// No Tenant, no labeled ClusterRoles/CRDs/ClusterRoleBindings in the fake store.
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS, TenantNamespace: tenantNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		// Finalizer removed → fake client GCs the Deployment.
		var updated appsv1.Deployment
		getErr := cli.Get(t.Context(), req.NamespacedName, &updated)
		g.Expect(getErr).Should(HaveOccurred())
		g.Expect(getErr.Error()).To(ContainSubstring("not found"))
	})

	t.Run("deletes Tenants then RBAC and CRDs when Deployment is terminating", func(t *testing.T) {
		g := NewWithT(t)
		now := metav1.NewTime(time.Now())
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: depName, Namespace: depNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{CleanupFinalizer},
		}}
		tenant := &maasv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNS,
		}}
		crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name: "maas-controller-crb", Labels: map[string]string(componentLabel),
		}}
		cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
			Name: "maas-controller-cr", Labels: map[string]string(componentLabel),
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep, tenant, crb, cr).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS, TenantNamespace: tenantNS}

		// First reconcile: Tenant exists — deleted, requeue while pending.
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res.RequeueAfter).To(Equal(requeueInterval))

		// Second reconcile: Tenant gone — RBAC cleaned, finalizer released, Deployment removed by GC.
		res, err = r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		crbList := &rbacv1.ClusterRoleBindingList{}
		g.Expect(cli.List(t.Context(), crbList, componentLabel)).To(Succeed())
		g.Expect(crbList.Items).To(BeEmpty())

		crList := &rbacv1.ClusterRoleList{}
		g.Expect(cli.List(t.Context(), crList, componentLabel)).To(Succeed())
		g.Expect(crList.Items).To(BeEmpty())

		// Deployment is fully removed once the finalizer is released.
		var updated appsv1.Deployment
		err = cli.Get(t.Context(), req.NamespacedName, &updated)
		g.Expect(err).Should(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("not found"))
	})
}
