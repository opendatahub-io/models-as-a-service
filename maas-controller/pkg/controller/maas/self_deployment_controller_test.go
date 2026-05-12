//nolint:testpackage
package maas

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	return s
}

func TestLifecycleReconciler(t *testing.T) {
	const (
		depName = "maas-controller"
		depNS   = "redhat-ods-applications"
	)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: depName, Namespace: depNS}}

	t.Run("does not add cleanup finalizer when Deployment is running", func(t *testing.T) {
		g := NewWithT(t)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: depNS}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		var updated appsv1.Deployment
		g.Expect(cli.Get(t.Context(), req.NamespacedName, &updated)).To(Succeed())
		g.Expect(updated.Finalizers).NotTo(ContainElement(CleanupFinalizer))
	})

	t.Run("strips legacy cleanup finalizer on running Deployment", func(t *testing.T) {
		g := NewWithT(t)
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: depName, Namespace: depNS,
			Finalizers: []string{CleanupFinalizer},
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		var updated appsv1.Deployment
		g.Expect(cli.Get(t.Context(), req.NamespacedName, &updated)).To(Succeed())
		g.Expect(updated.Finalizers).NotTo(ContainElement(CleanupFinalizer))
	})

	t.Run("sets Config owner reference when Config exists", func(t *testing.T) {
		g := NewWithT(t)
		scheme := selfDepScheme(t)
		cfg := &maasv1alpha1.Config{
			TypeMeta: metav1.TypeMeta{
				APIVersion: maasv1alpha1.GroupVersion.String(),
				Kind:       maasv1alpha1.ConfigKind,
			},
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: depNS}}
		cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, cfg).Build()

		r := &LifecycleReconciler{Client: cli, Scheme: scheme, DeploymentName: depName, DeploymentNS: depNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		var updated appsv1.Deployment
		g.Expect(cli.Get(t.Context(), req.NamespacedName, &updated)).To(Succeed())
		g.Expect(updated.OwnerReferences).ToNot(BeEmpty())
		g.Expect(updated.OwnerReferences[0].UID).To(Equal(types.UID("cfg-uid")))
		g.Expect(updated.OwnerReferences[0].Kind).To(Equal(maasv1alpha1.ConfigKind))
	})

	t.Run("no-op when Deployment is terminating but legacy CleanupFinalizer is absent", func(t *testing.T) {
		g := NewWithT(t)
		const placeholderFinalizer = "test/placeholder"
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:       depName,
			Namespace:  depNS,
			Finalizers: []string{placeholderFinalizer},
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()
		g.Expect(cli.Delete(t.Context(), dep)).To(Succeed())

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))
	})

	t.Run("removes legacy CleanupFinalizer when Deployment is terminating", func(t *testing.T) {
		g := NewWithT(t)
		now := metav1.NewTime(time.Now())
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: depName, Namespace: depNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{CleanupFinalizer},
		}}
		cli := fake.NewClientBuilder().WithScheme(selfDepScheme(t)).WithObjects(dep).Build()

		r := &LifecycleReconciler{Client: cli, DeploymentName: depName, DeploymentNS: depNS}
		res, err := r.Reconcile(t.Context(), req)
		g.Expect(err).ShouldNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		var updated appsv1.Deployment
		getErr := cli.Get(t.Context(), req.NamespacedName, &updated)
		if getErr == nil {
			g.Expect(updated.Finalizers).NotTo(ContainElement(CleanupFinalizer))
		} else {
			g.Expect(getErr.Error()).To(ContainSubstring("not found"))
		}
	})
}
