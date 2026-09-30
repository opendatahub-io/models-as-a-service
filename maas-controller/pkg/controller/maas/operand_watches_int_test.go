package maas

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	autov1 "k8s.io/api/autoscaling/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/controller/maas/fixture"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const operandWatchDefaultTenantNamespace = "models-as-a-service"

// operandWatchTenantNamespace is the namespace of the tenant config an operand's
// tracking labels name.
func operandWatchTenantNamespace(tenant string) string {
	return tenantreconcile.TenantNamespaceForAITenant(tenant, operandWatchDefaultTenantNamespace)
}

var _ = Describe("Tenant operand watches", func() {
	var (
		r                *TenantReconciler
		appNamespace     string
		gatewayNamespace string
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest).Name
		gatewayNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		r = &TenantReconciler{
			Client:                          envTest.Client,
			AppNamespace:                    appNamespace,
			GatewayNamespace:                gatewayNamespace,
			OperatorNamespace:               appNamespace,
			TenantNamespace:                 operandWatchDefaultTenantNamespace,
			TenantNamespaceDiscoveryEnabled: true,
		}
	})

	// stamp sets the tracking labels ApplyRendered writes when tenant applies obj.
	stamp := func(obj client.Object, tenant string) {
		tenantreconcile.SetTenantTrackingLabels(obj, &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: operandWatchTenantNamespace(tenant),
			Labels:    map[string]string{tenantreconcile.LabelTenantName: tenant},
		}})
	}

	// ownerTenantID is the identifier rendered names carry for tenant; the default tenant
	// keeps the base names.
	ownerTenantID := func(tenant string) string {
		if tenant == tenantreconcile.DefaultAITenantName {
			return ""
		}
		return tenant
	}

	// ensureTenantConfig creates tenant's config once; its namespace name is fixed, and
	// envtest never finishes deleting namespaces.
	ensureTenantConfig := func(ctx SpecContext, tenant string) {
		namespace := operandWatchTenantNamespace(tenant)
		Expect(client.IgnoreAlreadyExists(envTest.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(envTest.Create(ctx, &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace,
		}}))).To(Succeed())
	}

	watchOf := func(obj client.Object) tenantOperand {
		gvk, err := apiutil.GVKForObject(obj, envTest.Environment.Scheme)
		Expect(err).NotTo(HaveOccurred())
		for _, op := range tenantOperands() {
			if op.gvk == gvk {
				return op
			}
		}
		Fail("no operand watch for " + gvk.String())
		return tenantOperand{}
	}

	// observe reads obj in the shape its watch delivers: typed, unstructured or metadata only.
	observe := func(ctx SpecContext, op tenantOperand, obj client.Object) client.Object {
		observed := op.newObject()
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(obj), observed)).To(Succeed())
		return observed
	}

	admitsUpdate := func(op tenantOperand, oldObj, newObj client.Object) bool {
		return r.operandPredicate(op).Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
	}

	tenantConfigOf := func(namespace string) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: namespace,
		}}}
	}

	DescribeTableSubtree("on a built-in operand",
		// build returns the operand and an edit of its spec or data.
		func(build func(namespace string) (client.Object, func()), shared bool) {
			var (
				op       tenantOperand
				operand  client.Object
				edit     func()
				original client.Object
			)

			BeforeEach(func(ctx SpecContext) {
				// A shared operand maps to its labelled tenant only while that tenant config
				// would re-apply it.
				ensureTenantConfig(ctx, "red")
				operand, edit = build(appNamespace)
				stamp(operand, "red")
				op = watchOf(operand)
				Expect(envTest.Create(ctx, operand)).To(Succeed())
				original = observe(ctx, op, operand)
			})

			When("its spec or data is edited", func() {
				It("re-enqueues the tenant config named in its tracking labels", func(ctx SpecContext) {
					edit()
					Expect(envTest.Update(ctx, operand)).To(Succeed())
					edited := observe(ctx, op, operand)

					Expect(admitsUpdate(op, original, edited)).To(BeTrue())
					Expect(r.operandMapper(op)(ctx, edited)).To(Equal(tenantConfigOf(operandWatchTenantNamespace("red"))))
				})
			})

			When("it is deleted", func() {
				It("re-enqueues the tenant config named in its tracking labels", func(ctx SpecContext) {
					Expect(envTest.Delete(ctx, operand)).To(Succeed())
					Expect(apierrors.IsNotFound(envTest.Get(ctx, client.ObjectKeyFromObject(operand), op.newObject()))).To(BeTrue())

					Expect(r.operandPredicate(op).Delete(event.DeleteEvent{Object: original})).To(BeTrue())
					Expect(r.operandMapper(op)(ctx, original)).To(Equal(tenantConfigOf(operandWatchTenantNamespace("red"))))
				})
			})

			if shared {
				When("another tenant's apply stamps its own tracking labels", func() {
					It("is not admitted", func(ctx SpecContext) {
						// Every tenant applies this name; re-enqueueing on the relabel would
						// bounce the reconcile between tenants.
						stamp(operand, "blue")
						Expect(envTest.Update(ctx, operand)).To(Succeed())
						relabelled := observe(ctx, op, operand)

						Expect(relabelled.GetResourceVersion()).NotTo(Equal(original.GetResourceVersion()))
						Expect(admitsUpdate(op, original, relabelled)).To(BeFalse())
					})
				})
			} else {
				When("its tracking labels are rewritten to another tenant", func() {
					It("re-enqueues both tenants, so the owner restores its labels", func(ctx SpecContext) {
						ensureTenantConfig(ctx, "blue")
						stamp(operand, "blue")
						Expect(envTest.Update(ctx, operand)).To(Succeed())
						relabelled := observe(ctx, op, operand)

						Expect(admitsUpdate(op, original, relabelled)).To(BeTrue())
						Expect(r.operandMapper(op)(ctx, original)).To(Equal(tenantConfigOf(operandWatchTenantNamespace("red"))))
						Expect(r.operandMapper(op)(ctx, relabelled)).To(Equal(tenantConfigOf(operandWatchTenantNamespace("blue"))))
					})
				})
			}
		},
		Entry("Deployment (typed, generation)", func(ns string) (client.Object, func()) {
			dep := fixture.OperandDeployment(ns, tenantreconcile.PayloadProcessingDeploymentName("red"))
			return dep, func() { dep.Spec.Template.Spec.Containers[0].Image = "registry.example.com/operand:2" }
		}, false),
		Entry("Service (metadata, any write)", func(ns string) (client.Object, func()) {
			svc := fixture.OperandService(ns, "maas-api-metrics")
			return svc, func() { svc.Spec.Ports[0].Port = 9443 }
		}, true),
		Entry("ServiceAccount (metadata, any write)", func(ns string) (client.Object, func()) {
			sa := fixture.OperandServiceAccount(ns, "maas-api")
			return sa, func() { sa.AutomountServiceAccountToken = ptr.To(false) }
		}, true),
		Entry("ConfigMap (typed, any write)", func(ns string) (client.Object, func()) {
			cm := fixture.OperandConfigMap(ns, "maas-parameters")
			return cm, func() { cm.Data["namespace"] = "edited" }
		}, true),
		Entry("ClusterRole (metadata, any write)", func(ns string) (client.Object, func()) {
			role := fixture.OperandClusterRole(ns)
			return role, func() { role.Rules[0].Verbs = []string{"get", "list"} }
		}, true),
		Entry("ClusterRoleBinding (typed, any write)", func(ns string) (client.Object, func()) {
			binding := fixture.OperandClusterRoleBinding(ns, ns)
			return binding, func() { binding.Subjects[0].Name = "edited" }
		}, true),
		Entry("CronJob (metadata, generation)", func(ns string) (client.Object, func()) {
			cronJob := fixture.OperandCronJob(ns, tenantreconcile.MaaSAPIKeyCleanupCronJobName("red"))
			return cronJob, func() { cronJob.Spec.Schedule = "30 * * * *" }
		}, false),
		Entry("HorizontalPodAutoscaler (typed, spec compare)", func(ns string) (client.Object, func()) {
			hpa := fixture.OperandHPA(ns, tenantreconcile.PayloadProcessingHPAName("red"), tenantreconcile.PayloadProcessingDeploymentName("red"))
			return hpa, func() { hpa.Spec.MaxReplicas = 5 }
		}, false),
	)

	DescribeTable("a tenant-specific operand relabelled to another tenant and then deleted",
		func(ctx SpecContext, owner string) {
			// Only the owner renders this name, so only the owner can recreate it. The delete
			// maps to whichever tenant the labels name last; the relabel has to reach the owner.
			svc := fixture.OperandService(appNamespace, tenantreconcile.MaaSAPIServiceName(ownerTenantID(owner)))
			stamp(svc, owner)
			op := watchOf(svc)
			Expect(envTest.Create(ctx, svc)).To(Succeed())
			original := observe(ctx, op, svc)

			stamp(svc, "blue")
			Expect(envTest.Update(ctx, svc)).To(Succeed())
			relabelled := observe(ctx, op, svc)
			Expect(envTest.Delete(ctx, svc)).To(Succeed())

			var enqueued []reconcile.Request
			if admitsUpdate(op, original, relabelled) {
				enqueued = append(enqueued, r.operandMapper(op)(ctx, original)...)
				enqueued = append(enqueued, r.operandMapper(op)(ctx, relabelled)...)
			}
			if r.operandPredicate(op).Delete(event.DeleteEvent{Object: relabelled}) {
				enqueued = append(enqueued, r.operandMapper(op)(ctx, relabelled)...)
			}

			Expect(enqueued).To(ContainElement(tenantConfigOf(operandWatchTenantNamespace(owner))[0]))
		},
		Entry("per-tenant name", "red"),
		Entry("default tenant base name", tenantreconcile.DefaultAITenantName),
	)

	Describe("an operand opted out with opendatahub.io/managed=false", func() {
		unmanaged := func(obj client.Object) {
			obj.SetAnnotations(map[string]string{tenantreconcile.AnnotationManaged: "false"})
		}

		It("ignores edits that leave generation alone", func(ctx SpecContext) {
			cm := fixture.OperandConfigMap(gatewayNamespace, "payload-processing-plugins")
			stamp(cm, "red")
			unmanaged(cm)
			op := watchOf(cm)
			Expect(envTest.Create(ctx, cm)).To(Succeed())
			original := observe(ctx, op, cm)

			cm.Data["namespace"] = "edited by an operator"
			Expect(envTest.Update(ctx, cm)).To(Succeed())

			Expect(admitsUpdate(op, original, observe(ctx, op, cm))).To(BeFalse())
		})

		It("admits a spec edit that bumps generation", func(ctx SpecContext) {
			// Apply leaves it alone, but readiness checks still read the spec.
			dep := fixture.OperandDeployment(gatewayNamespace, "payload-processing")
			stamp(dep, "red")
			unmanaged(dep)
			op := watchOf(dep)
			Expect(envTest.Create(ctx, dep)).To(Succeed())
			original := observe(ctx, op, dep)

			dep.Spec.Template.Spec.Containers[0].Image = "registry.example.com/operand:2"
			Expect(envTest.Update(ctx, dep)).To(Succeed())
			edited := observe(ctx, op, dep)

			Expect(edited.GetGeneration()).To(BeNumerically(">", original.GetGeneration()))
			Expect(admitsUpdate(op, original, edited)).To(BeTrue())
		})

		It("admits opting out", func(ctx SpecContext) {
			cm := fixture.OperandConfigMap(gatewayNamespace, "payload-processing-plugins")
			stamp(cm, "red")
			op := watchOf(cm)
			Expect(envTest.Create(ctx, cm)).To(Succeed())
			original := observe(ctx, op, cm)

			unmanaged(cm)
			Expect(envTest.Update(ctx, cm)).To(Succeed())

			Expect(admitsUpdate(op, original, observe(ctx, op, cm))).To(BeTrue())
		})

		It("admits opting back in", func(ctx SpecContext) {
			cm := fixture.OperandConfigMap(gatewayNamespace, "payload-processing-plugins")
			stamp(cm, "red")
			unmanaged(cm)
			op := watchOf(cm)
			Expect(envTest.Create(ctx, cm)).To(Succeed())
			original := observe(ctx, op, cm)

			cm.SetAnnotations(nil)
			Expect(envTest.Update(ctx, cm)).To(Succeed())

			Expect(admitsUpdate(op, original, observe(ctx, op, cm))).To(BeTrue())
		})
	})

	Describe("a HorizontalPodAutoscaler", func() {
		var (
			hpa      *autov2.HorizontalPodAutoscaler
			op       tenantOperand
			original client.Object
		)

		BeforeEach(func(ctx SpecContext) {
			hpa = fixture.OperandHPA(gatewayNamespace, "payload-processing", "payload-processing")
			stamp(hpa, "red")
			op = watchOf(hpa)
			Expect(envTest.Create(ctx, hpa)).To(Succeed())
			original = observe(ctx, op, hpa)
		})

		It("admits a spec edit although generation stays at 0", func(ctx SpecContext) {
			hpa.Spec.MaxReplicas = 5
			Expect(envTest.Update(ctx, hpa)).To(Succeed())
			edited := observe(ctx, op, hpa)

			Expect(original.GetGeneration()).To(BeZero())
			Expect(edited.GetGeneration()).To(BeZero())
			Expect(admitsUpdate(op, original, edited)).To(BeTrue())
		})

		It("ignores status written by the autoscaler", func(ctx SpecContext) {
			hpa.Status = autov2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2, DesiredReplicas: 2}
			Expect(envTest.Status().Update(ctx, hpa)).To(Succeed())
			scaled := observe(ctx, op, hpa)

			Expect(scaled.GetResourceVersion()).NotTo(Equal(original.GetResourceVersion()))
			Expect(admitsUpdate(op, original, scaled)).To(BeFalse())
		})
	})

	Describe("a Deployment an HPA can scale", func() {
		var (
			dep      *appsv1.Deployment
			op       tenantOperand
			original client.Object
		)

		BeforeEach(func(ctx SpecContext) {
			dep = fixture.OperandDeployment(gatewayNamespace, "payload-processing")
			stamp(dep, "red")
			op = watchOf(dep)
			Expect(envTest.Create(ctx, dep)).To(Succeed())
			original = observe(ctx, op, dep)
		})

		scaleTo := func(ctx SpecContext, replicas int32) client.Object {
			scale := &autov1.Scale{
				ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace},
				Spec:       autov1.ScaleSpec{Replicas: replicas},
			}
			Expect(envTest.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale))).To(Succeed())
			scaled := observe(ctx, op, dep)
			Expect(scaled.GetGeneration()).To(BeNumerically(">", original.GetGeneration()))
			return scaled
		}

		It("ignores a replicas-only change while an HPA targets it", func(ctx SpecContext) {
			Expect(envTest.Create(ctx, fixture.OperandHPA(gatewayNamespace, "payload-processing", dep.Name))).To(Succeed())

			Expect(admitsUpdate(op, original, scaleTo(ctx, 3))).To(BeFalse())
		})

		It("admits the same change without an HPA", func(ctx SpecContext) {
			Expect(admitsUpdate(op, original, scaleTo(ctx, 3))).To(BeTrue())
		})
	})

	Describe("the maas-api Deployment", func() {
		var (
			dep      *appsv1.Deployment
			op       tenantOperand
			original client.Object
		)

		BeforeEach(func(ctx SpecContext) {
			dep = fixture.OperandDeployment(appNamespace, tenantreconcile.DefaultMaaSAPIDeploymentName)
			stamp(dep, "red")
			op = watchOf(dep)
			Expect(envTest.Create(ctx, dep)).To(Succeed())
			original = observe(ctx, op, dep)
		})

		rolloutStatus := func(ctx SpecContext, available int32) client.Object {
			dep.Status = appsv1.DeploymentStatus{
				ObservedGeneration: dep.Generation,
				Replicas:           1,
				UpdatedReplicas:    1,
				AvailableReplicas:  available,
				ReadyReplicas:      available,
			}
			Expect(envTest.Status().Update(ctx, dep)).To(Succeed())
			updated := observe(ctx, op, dep)
			Expect(updated.GetGeneration()).To(Equal(original.GetGeneration()))
			return updated
		}

		It("admits the status update that makes it available", func(ctx SpecContext) {
			available := rolloutStatus(ctx, 1)

			Expect(admitsUpdate(op, original, available)).To(BeTrue())
			Expect(r.operandMapper(op)(ctx, available)).To(Equal(tenantConfigOf(operandWatchTenantNamespace("red"))))
		})

		It("ignores status churn that stays below the readiness gate", func(ctx SpecContext) {
			Expect(admitsUpdate(op, original, rolloutStatus(ctx, 0))).To(BeFalse())
		})
	})

	Describe("an unlabelled maas-api Deployment owned by another controller", func() {
		createUnlabelled := func(ctx SpecContext, name string) (tenantOperand, client.Object) {
			dep := fixture.OperandDeployment(appNamespace, name)
			op := watchOf(dep)
			Expect(envTest.Create(ctx, dep)).To(Succeed())
			return op, observe(ctx, op, dep)
		}

		It("maps the default name to the default tenant config", func(ctx SpecContext) {
			op, dep := createUnlabelled(ctx, tenantreconcile.DefaultMaaSAPIDeploymentName)

			Expect(r.operandPredicate(op).Create(event.CreateEvent{Object: dep})).To(BeTrue())
			Expect(r.operandMapper(op)(ctx, dep)).To(Equal(tenantConfigOf(operandWatchDefaultTenantNamespace)))
		})

		It("does not guess the tenant of a per-tenant name", func(ctx SpecContext) {
			op, dep := createUnlabelled(ctx, tenantreconcile.DefaultMaaSAPIDeploymentName+"-red")

			Expect(r.operandMapper(op)(ctx, dep)).To(BeEmpty())
		})
	})
})

// These specs run the operand watches in a manager against a real API server, with PR1's
// recorder in place of the platform reconcile.
var _ = Describe("Tenant operand watches through a manager", func() {
	var (
		appNamespace     string
		gatewayNamespace string
		survivor         reconcile.Request
		recorder         *tenantRequestRecorder
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("app")).Name
		gatewayNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		survivor = predTenantRequest(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("survivor")).Name)[0]
		Expect(envTest.Create(ctx, predTenantConfig(survivor.Namespace))).To(Succeed())
		recorder = newTenantRequestRecorder(func(context.Context, reconcile.Request) error { return nil })
	})

	// stampedFor sets the tracking labels the tenant config in tenantNamespace applies with.
	stampedFor := func(obj client.Object, tenantNamespace string) client.Object {
		tenantreconcile.SetTenantTrackingLabels(obj, predTenantConfig(tenantNamespace))
		return obj
	}

	startWatches := func(ctx SpecContext) {
		GinkgoHelper()

		r := &TenantReconciler{
			AppNamespace:                    appNamespace,
			OperatorNamespace:               appNamespace,
			TenantNamespace:                 survivor.Namespace,
			GatewayNamespace:                gatewayNamespace,
			TenantNamespaceDiscoveryEnabled: true,
		}
		startWatchManager(ctx, func(mgr ctrl.Manager) error {
			r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
			return r.setupWithManager(mgr, recorder)
		})
		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElement(survivor))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
	}

	It("wakes the remaining tenants when a shared operand last applied by a departed tenant is deleted", func(ctx SpecContext) {
		// Shared operands keep whichever tenant applied them last; once that tenant is gone,
		// only a remaining tenant can recreate what every tenant's maas-api needs.
		sa := stampedFor(fixture.OperandServiceAccount(appNamespace, "maas-api"), operandWatchTenantNamespace("departed"))
		Expect(envTest.Create(ctx, sa)).To(Succeed())
		startWatches(ctx)
		mark := recorder.mark()

		Expect(envTest.Delete(ctx, sa)).To(Succeed())

		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(survivor))
	})
})
