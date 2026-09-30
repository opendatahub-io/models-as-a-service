package maas

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	autov1 "k8s.io/api/autoscaling/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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

// markShared sets the marker PostRender's final pass stamps on an object no rename site
// claimed. Real shared operands never carry tracking labels.
func markShared(obj client.Object) client.Object {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[tenantreconcile.LabelSharedOperand] = "true"
	obj.SetLabels(l)
	return obj
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

	// stamp sets the tracking labels a rename site writes when tenant applies obj.
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
				// A shared operand maps to a live, reconcilable tenant
				// (enqueueSharedOperandRepairer); with only "red" configured, that is red.
				ensureTenantConfig(ctx, "red")
				operand, edit = build(appNamespace)
				if shared {
					markShared(operand)
				} else {
					stamp(operand, "red")
				}
				op = watchOf(operand)
				Expect(envTest.Create(ctx, operand)).To(Succeed())
				original = observe(ctx, op, operand)
			})

			// wantMapped is what operandMapper should return for a marker-carrying obj: not
			// a hardcoded tenant (other specs' tenant configs accumulate cluster-wide across
			// this suite, since envtest never finishes deleting a namespace, so the winner
			// enqueueSharedOperandRepairer picks among them is not fixed), but exactly what
			// that repair-selection helper itself picks. The helper's own selection logic
			// (default preferred, deterministic fallback, none qualifying) is unit-tested
			// against a clean fake client in TestOperandMapperForSharedOperands.
			wantMapped := func(ctx SpecContext, obj client.Object) []reconcile.Request {
				if shared {
					return r.enqueueSharedOperandRepairer(ctx, obj)
				}
				return tenantConfigOf(operandWatchTenantNamespace("red"))
			}

			When("its spec or data is edited", func() {
				It("re-enqueues the tenant config named in its tracking labels", func(ctx SpecContext) {
					edit()
					Expect(envTest.Update(ctx, operand)).To(Succeed())
					edited := observe(ctx, op, operand)

					Expect(admitsUpdate(op, original, edited)).To(BeTrue())
					Expect(r.operandMapper(op)(ctx, edited)).To(Equal(wantMapped(ctx, edited)))
				})
			})

			When("it is deleted", func() {
				It("re-enqueues the tenant config named in its tracking labels", func(ctx SpecContext) {
					Expect(envTest.Delete(ctx, operand)).To(Succeed())
					Expect(apierrors.IsNotFound(envTest.Get(ctx, client.ObjectKeyFromObject(operand), op.newObject()))).To(BeTrue())

					Expect(r.operandPredicate(op).Delete(event.DeleteEvent{Object: original})).To(BeTrue())
					Expect(r.operandMapper(op)(ctx, original)).To(Equal(wantMapped(ctx, original)))
				})
			})

			if shared {
				When("it also gains a tenant's tracking labels", func() {
					It("is admitted: relabel suppression no longer exists", func(ctx SpecContext) {
						// A real shared operand never carries tracking labels post-render
						// (it gets the marker instead), so this cannot happen through the
						// pipeline. It probes that the predicate no longer special-cases a
						// label change on a marked object either way.
						stamp(operand, "blue")
						Expect(envTest.Update(ctx, operand)).To(Succeed())
						relabelled := observe(ctx, op, operand)

						Expect(relabelled.GetResourceVersion()).NotTo(Equal(original.GetResourceVersion()))
						Expect(admitsUpdate(op, original, relabelled)).To(BeTrue())
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

	// startWatches is startWatchManager with the cache the manager binary builds: the
	// operand scopes and managed fields stripped. It returns that cache.
	startWatches := func(ctx SpecContext) cache.Cache {
		GinkgoHelper()

		scopes, err := client.New(envTest.Config, client.Options{Scheme: watchScheme()})
		Expect(err).NotTo(HaveOccurred())
		mgr, err := ctrl.NewManager(envTest.Config, ctrl.Options{
			Scheme: watchScheme(),
			Cache: cache.Options{
				ByObject:         TenantOperandCacheByObject(ctx, scopes, appNamespace, gatewayNamespace),
				DefaultTransform: cache.TransformStripManagedFields(),
			},
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &TenantReconciler{
			Client:                          mgr.GetClient(),
			Scheme:                          mgr.GetScheme(),
			AppNamespace:                    appNamespace,
			OperatorNamespace:               appNamespace,
			TenantNamespace:                 survivor.Namespace,
			GatewayNamespace:                gatewayNamespace,
			TenantNamespaceDiscoveryEnabled: true,
		}
		Expect(r.setupWithManager(mgr, recorder)).To(Succeed())

		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(mgr.Start(ctx)).To(Succeed())
		}()
		DeferCleanup(func() { Eventually(stopped).Should(BeClosed()) })
		Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElement(survivor))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		return mgr.GetCache()
	}

	since := func(mark int) func() []reconcile.Request {
		return func() []reconcile.Request { return recorder.since(mark) }
	}

	It("repairs a deleted shared operand through the surviving tenant", func(ctx SpecContext) {
		// A shared operand carries only the marker, never a "last applier" to lose:
		// enqueueSharedOperandRepairer picks survivor because it is the only (and the
		// default) live, reconcilable tenant config.
		sa := markShared(fixture.OperandServiceAccount(appNamespace, "maas-api"))
		Expect(envTest.Create(ctx, sa)).To(Succeed())
		startWatches(ctx)
		mark := recorder.mark()

		Expect(envTest.Delete(ctx, sa)).To(Succeed())

		Eventually(since(mark)).Should(ContainElement(survivor))
	})

	It("delivers operand events through the namespace-scoped metadata cache", func(ctx SpecContext) {
		cached := startWatches(ctx)
		mark := recorder.mark()

		Expect(envTest.Create(ctx, stampedFor(fixture.OperandServiceAccount(appNamespace, "maas-api"), survivor.Namespace))).To(Succeed())

		Eventually(since(mark)).Should(ContainElement(survivor))
		// The scope is in force: the cache holds no ServiceAccounts outside the app and
		// gateway namespaces.
		outside := &metav1.PartialObjectMetadata{}
		outside.SetGroupVersionKind(tenantreconcile.GVKServiceAccount)
		err := cached.Get(ctx, client.ObjectKey{Namespace: survivor.Namespace, Name: "default"}, outside)
		Expect(err).To(MatchError(ContainSubstring("unknown namespace")))
	})

	It("sees a ClusterRole that loses its tracking labels as a delete carrying the old labels", func(ctx SpecContext) {
		role := stampedFor(fixture.OperandClusterRole(appNamespace), survivor.Namespace)
		Expect(envTest.Create(ctx, role)).To(Succeed())
		cached := startWatches(ctx)
		mark := recorder.mark()

		role.SetLabels(nil)
		Expect(envTest.Update(ctx, role)).To(Succeed())

		// The label-scoped informer drops the object; only its old labels name the tenant.
		Eventually(since(mark)).Should(ContainElement(survivor))
		gone := &metav1.PartialObjectMetadata{}
		gone.SetGroupVersionKind(gvkClusterRole)
		Expect(apierrors.IsNotFound(cached.Get(ctx, client.ObjectKeyFromObject(role), gone))).To(BeTrue())
	})

	It("registers a CRD-backed operand watch once the CRD is served, and wakes the tenants", func(ctx SpecContext) {
		Expect(envTest.Serves(tenantreconcile.GVKCertificate)).To(BeFalse(), "another spec installed the Certificate CRD")
		startWatches(ctx)
		mark := recorder.mark()

		// A CRD with no objects yet produces no operand event of its own.
		_, err := envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{CRDs: []*extv1.CustomResourceDefinition{{
			ObjectMeta: metav1.ObjectMeta{Name: "certificates.cert-manager.io"},
			Spec: extv1.CustomResourceDefinitionSpec{
				Group: tenantreconcile.GVKCertificate.Group,
				Names: extv1.CustomResourceDefinitionNames{Plural: "certificates", Singular: "certificate", Kind: "Certificate", ListKind: "CertificateList"},
				Scope: extv1.NamespaceScoped,
				Versions: []extv1.CustomResourceDefinitionVersion{{
					Name: tenantreconcile.GVKCertificate.Version, Served: true, Storage: true,
					Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
						Type: "object", XPreserveUnknownFields: ptr.To(true),
					}},
				}},
			},
		}}})
		Expect(err).NotTo(HaveOccurred())
		Eventually(since(mark)).Should(ContainElement(survivor))

		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		mark = recorder.mark()
		certificate := &unstructured.Unstructured{}
		certificate.SetGroupVersionKind(tenantreconcile.GVKCertificate)
		certificate.SetNamespace(appNamespace)
		certificate.SetName(tenantreconcile.MaaSAPIServingCertName(""))
		Expect(envTest.Create(ctx, stampedFor(certificate, survivor.Namespace))).To(Succeed())

		Eventually(since(mark)).Should(ContainElement(survivor))
	})

	Describe("stays quiet", func() {
		It("on status the autoscaler writes", func(ctx SpecContext) {
			hpa := fixture.OperandHPA(gatewayNamespace, "payload-processing", "payload-processing")
			stampedFor(hpa, survivor.Namespace)
			Expect(envTest.Create(ctx, hpa)).To(Succeed())
			startWatches(ctx)
			mark := recorder.mark()

			hpa.Status = autov2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2, DesiredReplicas: 3}
			Expect(envTest.Status().Update(ctx, hpa)).To(Succeed())

			Consistently(since(mark)).WithTimeout(watchQuietPeriod).Should(BeEmpty())
		})

	})

	// A shared operand renders byte-identical for every tenant (see the design doc), so
	// its watch event only needs to repair it once, not once per tenant: fanning out to
	// every live tenant would multiply a single external write by N for no gain, which
	// matters because ai-gateway-controller writes the shared payload-processing-reader
	// ClusterRole on every praxis tenant's own resync.
	It("enqueues exactly one tenant when a shared operand is written by another field manager", func(ctx SpecContext) {
		other := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("other")).Name
		Expect(envTest.Create(ctx, predTenantConfig(other))).To(Succeed())
		role := fixture.OperandClusterRole(appNamespace)
		markShared(role)
		Expect(envTest.Create(ctx, role)).To(Succeed())
		startWatches(ctx)
		mark := recorder.mark()

		role.Rules[0].Verbs = []string{"get", "list"}
		Expect(envTest.Update(ctx, role)).To(Succeed())

		Eventually(since(mark)).Should(ContainElement(survivor))
		Consistently(since(mark)).WithTimeout(watchQuietPeriod).Should(ConsistOf(survivor))
	})
})
