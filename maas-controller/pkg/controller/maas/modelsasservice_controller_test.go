//nolint:testpackage
package maas

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	componentsv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/components/v1alpha1"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func modelsAsServiceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(componentsv1alpha1.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func TestModelsAsServiceReconciler_AllHealthyTenantsReportReady(t *testing.T) {
	g := NewWithT(t)
	s := modelsAsServiceTestScheme(t)

	module := &componentsv1alpha1.ModelsAsService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       componentsv1alpha1.ModelsAsServiceInstanceName,
			Generation: 3,
		},
	}
	tenantA := tenantWithModuleStatus("models-as-a-service", "Active",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionTrue, "Reconciled", "healthy"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionTrue, "DeploymentsReady", "healthy"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "PrerequisitesMet", "healthy"),
	)
	tenantB := tenantWithModuleStatus("team-a-maas", "Active",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionTrue, "Reconciled", "healthy"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionTrue, "DeploymentsReady", "healthy"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "PrerequisitesMet", "healthy"),
	)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&componentsv1alpha1.ModelsAsService{}, &maasv1alpha1.Tenant{}).
		WithObjects(module, tenantA, tenantB).
		Build()

	r := &ModelsAsServiceReconciler{Client: cl}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated componentsv1alpha1.ModelsAsService
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName}, &updated)).To(Succeed())
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(3)))
	g.Expect(updated.Status.Phase).To(Equal(componentsv1alpha1.ModelsAsServicePhaseReady))
	g.Expect(updated.Status.Releases).To(ContainElements(
		componentsv1alpha1.ComponentRelease{Name: "Models as a Service", RepoURL: modelsAsServiceRepoURL},
	))

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Message).To(Equal("MaaS is ready in all 2 tenant namespaces."))

	provisioning := apimeta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
	g.Expect(provisioning).NotTo(BeNil())
	g.Expect(provisioning.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(provisioning.Message).To(Equal("MaaS setup has completed in all 2 tenant namespaces."))

	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(degraded.Message).To(Equal("MaaS is operating normally in all 2 tenant namespaces."))
}

func TestModelsAsServiceReconciler_DegradedTenantKeepsModuleReady(t *testing.T) {
	g := NewWithT(t)
	s := modelsAsServiceTestScheme(t)

	module := &componentsv1alpha1.ModelsAsService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       componentsv1alpha1.ModelsAsServiceInstanceName,
			Generation: 7,
		},
	}
	healthyTenant := tenantWithModuleStatus("models-as-a-service", "Active",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionTrue, "Reconciled", "healthy"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionTrue, "DeploymentsReady", "healthy"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "PrerequisitesMet", "healthy"),
	)
	degradedTenant := tenantWithModuleStatus("team-b-maas", "Degraded",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionTrue, "Reconciled", "healthy but degraded"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionTrue, "DeploymentsReady", "healthy but degraded"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionTrue, "PrerequisitesWarning", "optional monitoring is missing"),
	)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&componentsv1alpha1.ModelsAsService{}, &maasv1alpha1.Tenant{}).
		WithObjects(module, healthyTenant, degradedTenant).
		Build()

	r := &ModelsAsServiceReconciler{Client: cl}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated componentsv1alpha1.ModelsAsService
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName}, &updated)).To(Succeed())
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(7)))
	g.Expect(updated.Status.Phase).To(Equal(componentsv1alpha1.ModelsAsServicePhaseReady))

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Message).To(Equal("MaaS is ready in all 2 tenant namespaces."))

	provisioning := apimeta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
	g.Expect(provisioning).NotTo(BeNil())
	g.Expect(provisioning.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(provisioning.Message).To(Equal("MaaS setup has completed in all 2 tenant namespaces."))

	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Message).To(Equal("MaaS is degraded in 1 of 2 tenant namespaces: team-b-maas"))
}

func TestModelsAsServiceReconciler_NotReadyTenantMakesModuleNotReady(t *testing.T) {
	g := NewWithT(t)
	s := modelsAsServiceTestScheme(t)

	module := &componentsv1alpha1.ModelsAsService{
		ObjectMeta: metav1.ObjectMeta{
			Name:       componentsv1alpha1.ModelsAsServiceInstanceName,
			Generation: 11,
		},
	}
	healthyTenant := tenantWithModuleStatus("models-as-a-service", "Active",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionTrue, "Reconciled", "healthy"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionTrue, "DeploymentsReady", "healthy"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "PrerequisitesMet", "healthy"),
	)
	notReadyTenant := tenantWithModuleStatus("team-c-maas", "Pending",
		newCondition(tenantreconcile.ReadyConditionType, metav1.ConditionFalse, "GatewayNotReady", "waiting for gateway"),
		newCondition(tenantreconcile.ConditionDeploymentsAvailable, metav1.ConditionFalse, "DeploymentsNotReady", "waiting for deployment"),
		newCondition(tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "PrerequisitesMet", "not degraded"),
	)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&componentsv1alpha1.ModelsAsService{}, &maasv1alpha1.Tenant{}).
		WithObjects(module, healthyTenant, notReadyTenant).
		Build()

	r := &ModelsAsServiceReconciler{Client: cl}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated componentsv1alpha1.ModelsAsService
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: componentsv1alpha1.ModelsAsServiceInstanceName}, &updated)).To(Succeed())
	g.Expect(updated.Status.ObservedGeneration).To(Equal(int64(11)))
	g.Expect(updated.Status.Phase).To(Equal(componentsv1alpha1.ModelsAsServicePhaseNotReady))

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(ready.Message).To(Equal("MaaS is not ready in 1 of 2 tenant namespaces: team-c-maas"))

	provisioning := apimeta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
	g.Expect(provisioning).NotTo(BeNil())
	g.Expect(provisioning.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioning.Message).To(Equal("MaaS setup is still in progress in 1 of 2 tenant namespaces: team-c-maas"))

	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
}

func tenantWithModuleStatus(namespace, phase string, conditions ...metav1.Condition) *maasv1alpha1.Tenant {
	return &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: namespace,
		},
		Status: maasv1alpha1.TenantStatus{
			Phase:      phase,
			Conditions: conditions,
		},
	}
}

func newCondition(typ string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}
}
