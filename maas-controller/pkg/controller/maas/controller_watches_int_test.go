package maas

import (
	"context"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	authPolicyGVK = schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}
	httpRouteGVK  = schema.GroupVersionKind{Group: gatewayapiv1.GroupName, Version: "v1", Kind: "HTTPRoute"}
	gatewayGVK    = schema.GroupVersionKind{Group: gatewayapiv1.GroupName, Version: "v1", Kind: "Gateway"}
)

var _ = Describe("MaaSAuthPolicyReconciler watches", func() {
	var (
		modelNamespace string
		policy         reconcile.Request
		recorder       *tenantRequestRecorder
		startWatches   func(SpecContext)
	)

	BeforeEach(func(ctx SpecContext) {
		// The AuthPolicy CRD is served before setup, so the watch registers statically.
		installSchemalessCRD(httpRouteGVK, "httproutes", extv1.NamespaceScoped)
		installSchemalessCRD(authPolicyGVK, "authpolicies", extv1.NamespaceScoped)

		tenantNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant")).Name
		modelNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("model")).Name
		Expect(envTest.Create(ctx, newMaaSModelRef("llm", modelNamespace, "ExternalModel", "llm"))).To(Succeed())
		authPolicy := newMaaSAuthPolicy("llm-access", tenantNamespace, "team-a", maasv1alpha1.ModelRef{Name: "llm", Namespace: modelNamespace})
		Expect(envTest.Create(ctx, authPolicy)).To(Succeed())
		policy = reconcile.Request{NamespacedName: types.NamespacedName{Name: authPolicy.Name, Namespace: tenantNamespace}}

		recorder = newTenantRequestRecorder(func(context.Context, reconcile.Request) error { return nil })
		startWatches = func(ctx SpecContext) {
			GinkgoHelper()

			r := &MaaSAuthPolicyReconciler{TenantNamespace: tenantNamespace}
			startWatchManager(ctx, func(mgr ctrl.Manager) error {
				r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
				return r.setupWithManager(mgr, recorder)
			})
			Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElement(policy))
			Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		}
	})

	It("re-runs conflict detection when a foreign AuthPolicy retargets, not on its status", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		foreign := &unstructured.Unstructured{}
		foreign.SetGroupVersionKind(authPolicyGVK)
		foreign.SetName("rogue")
		foreign.SetNamespace(modelNamespace)
		targetRef := map[string]any{"group": gatewayapiv1.GroupName, "kind": "HTTPRoute", "name": "llm"}
		Expect(unstructured.SetNestedMap(foreign.Object, targetRef, "spec", "targetRef")).To(Succeed())
		Expect(envTest.Create(ctx, foreign)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(policy))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark = recorder.mark()
		conditions := []any{map[string]any{"type": "Enforced", "status": "True", "reason": "Enforced"}}
		Expect(unstructured.SetNestedSlice(foreign.Object, conditions, "status", "conditions")).To(Succeed())
		Expect(envTest.Status().Update(ctx, foreign)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(policy))

		Expect(unstructured.SetNestedField(foreign.Object, "llm-v2", "spec", "targetRef", "name")).To(Succeed())
		Expect(envTest.Update(ctx, foreign)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(policy))
	})

	It("drops HTTPRoute status writes but not label changes", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(httpRouteGVK)
		route.SetName("llm")
		route.SetNamespace(modelNamespace)
		Expect(envTest.Create(ctx, route)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(policy))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark = recorder.mark()
		parents := []any{map[string]any{
			"parentRef":      map[string]any{"name": "maas-default-gateway"},
			"controllerName": "kuadrant.io/policy-controller",
		}}
		Expect(unstructured.SetNestedSlice(route.Object, parents, "status", "parents")).To(Succeed())
		Expect(envTest.Status().Update(ctx, route)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(policy))

		route.SetLabels(map[string]string{"app.kubernetes.io/name": "llm"})
		Expect(envTest.Update(ctx, route)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(policy))
	})
})

var _ = Describe("LifecycleReconciler watches", func() {
	It("re-enqueues on default tenant readiness changes, not on other status", func(ctx SpecContext) {
		controllerNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("controller")).Name
		tenantNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant")).Name
		self := reconcile.Request{NamespacedName: types.NamespacedName{Name: "maas-controller", Namespace: controllerNamespace}}
		recorder := newTenantRequestRecorder(func(context.Context, reconcile.Request) error { return nil })
		r := &LifecycleReconciler{
			DeploymentName:              self.Name,
			DeploymentNS:                self.Namespace,
			TenantSubscriptionNamespace: tenantNamespace,
			MonitoringNamespace:         controllerNamespace,
		}
		startWatchManager(ctx, func(mgr ctrl.Manager) error {
			r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
			return r.setupWithManager(mgr, recorder)
		})

		tenantConfig := predTenantConfig(tenantNamespace)
		Expect(envTest.Create(ctx, tenantConfig)).To(Succeed())
		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElement(self))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark := recorder.mark()
		tenantConfig.Status.InfraNamespace = "opendatahub"
		Expect(envTest.Status().Update(ctx, tenantConfig)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(self))

		apimeta.SetStatusCondition(&tenantConfig.Status.Conditions, metav1.Condition{
			Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionTrue,
			Reason: "Reconciled", ObservedGeneration: tenantConfig.Generation,
		})
		Expect(envTest.Status().Update(ctx, tenantConfig)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(self))
	})
})
