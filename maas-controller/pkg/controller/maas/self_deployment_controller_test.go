//nolint:testpackage
package maas

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

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
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: tenantNS,
		},
	}

	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, cfg, tenant).Build()
	r := &LifecycleReconciler{
		Client:                      cl,
		Scheme:                      s,
		DeploymentName:              "maas-controller",
		DeploymentNS:                depNS,
		TenantSubscriptionNamespace: tenantNS,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: depNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNS}, &updated)).To(Succeed())
	g.Expect(updated.OwnerReferences).ToNot(BeEmpty())
	ref := updated.OwnerReferences[0]
	g.Expect(ref.UID).To(Equal(types.UID("cfg-uid-tenant")))
	g.Expect(ref.Kind).To(Equal(maasv1alpha1.ConfigKind))
	g.Expect(ref.Controller).To(BeNil())
}
