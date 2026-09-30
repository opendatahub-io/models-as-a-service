package maas

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// predHoldFinalizer keeps a deleted object around so the update that sets
// deletionTimestamp is observable.
const predHoldFinalizer = "test.maas.opendatahub.io/hold"

// Each spec reads the stored object, makes one real write and reads it again. The two
// reads are what an informer hands a predicate as the old and new object of an update.
var _ = Describe("Tenant watch predicates", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = pkgtest.NewTestNamespace(ctx, envTest).Name
	})

	Describe("TenantReconciler MaasTenantConfig watch", func() {
		var tenantConfig *maasv1alpha1.MaasTenantConfig

		BeforeEach(func(ctx SpecContext) {
			tenantConfig = predTenantConfig(namespace)
			tenantConfig.Labels = map[string]string{tenantreconcile.LabelTenantName: "default"}
			tenantConfig.Annotations = map[string]string{managementStateAnnotation: managementStateManaged}
			Expect(envTest.Create(ctx, tenantConfig)).To(Succeed())
		})

		When("only status is written", func() {
			It("does not admit the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
					current.Status.Phase = "Active"
					current.Status.InfraNamespace = "opendatahub"
					Expect(envTest.Status().Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.ResourceVersion).NotTo(Equal(oldObj.ResourceVersion))
				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				Expect(predAdmits(tenantConfigChangedForTenant(), oldObj, newObj)).To(BeFalse())
			})
		})

		DescribeTable("admits writes the reconcile reads",
			func(ctx SpecContext, mutate func(*maasv1alpha1.MaasTenantConfig), bumpsGeneration bool) {
				oldObj, newObj := predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
					mutate(current)
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation > oldObj.Generation).To(Equal(bumpsGeneration))
				Expect(predAdmits(tenantConfigChangedForTenant(), oldObj, newObj)).To(BeTrue())
			},
			Entry("a management-state annotation change", func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.Annotations[managementStateAnnotation] = managementStateRemoved
			}, false),
			Entry("a tenant label change", func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.Labels[tenantreconcile.LabelTenantName] = "team-a"
			}, false),
			Entry("a spec change", func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.Spec.APIKeys = &maasv1alpha1.TenantAPIKeysConfig{MaxExpirationDays: ptr.To[int32](30)}
			}, true),
		)

		When("an object held by a finalizer is deleted", func() {
			It("admits the update that sets deletionTimestamp", func(ctx SpecContext) {
				predAddFinalizer(ctx, tenantConfig)

				oldObj, newObj := predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
					Expect(envTest.Delete(ctx, current)).To(Succeed())
				})

				Expect(newObj.DeletionTimestamp).NotTo(BeNil())
				Expect(newObj.Generation).To(BeNumerically(">", oldObj.Generation))
				Expect(predAdmits(tenantConfigChangedForTenant(), oldObj, newObj)).To(BeTrue())
			})
		})

		When("the informer resyncs", func() {
			It("admits the unchanged object", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, tenantConfig, func(context.Context, *maasv1alpha1.MaasTenantConfig) {})

				Expect(newObj.ResourceVersion).To(Equal(oldObj.ResourceVersion))
				Expect(predAdmits(tenantConfigChangedForTenant(), oldObj, newObj)).To(BeTrue())
			})
		})
	})

	Describe("TenantReconciler AITenant watch", func() {
		var aitenant *maasv1alpha1.AITenant

		BeforeEach(func(ctx SpecContext) {
			aitenant = &maasv1alpha1.AITenant{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: namespace}}
			Expect(envTest.Create(ctx, aitenant)).To(Succeed())
			predWrite(ctx, aitenant, func(ctx context.Context, current *maasv1alpha1.AITenant) {
				current.Status.Phase = "Pending"
				current.Status.GatewayRef = maasv1alpha1.TenantGatewayRef{Name: "team-a", Namespace: "openshift-ingress"}
				Expect(envTest.Status().Update(ctx, current)).To(Succeed())
			})
		})

		When("status.gatewayRef changes", func() {
			It("admits the status-only update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, aitenant, func(ctx context.Context, current *maasv1alpha1.AITenant) {
					current.Status.GatewayRef.Name = "team-a-v2"
					Expect(envTest.Status().Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				Expect(predAdmits(aitenantPlatformContextChanged(), oldObj, newObj)).To(BeTrue())
			})
		})

		When("other status fields change", func() {
			It("does not admit the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, aitenant, func(ctx context.Context, current *maasv1alpha1.AITenant) {
					current.Status.Phase = "Active"
					apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
						Type: maasv1alpha1.AITenantConditionReady, Status: metav1.ConditionTrue, Reason: "Reconciled",
					})
					Expect(envTest.Status().Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.ResourceVersion).NotTo(Equal(oldObj.ResourceVersion))
				Expect(predAdmits(aitenantPlatformContextChanged(), oldObj, newObj)).To(BeFalse())
			})
		})

		When("spec.oidc changes", func() {
			It("admits the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, aitenant, func(ctx context.Context, current *maasv1alpha1.AITenant) {
					current.Spec.OIDC = &maasv1alpha1.TenantExternalOIDCConfig{
						IssuerURL: "https://keycloak.example.com/realms/maas",
						ClientID:  "maas",
					}
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(BeNumerically(">", oldObj.Generation))
				Expect(predAdmits(aitenantPlatformContextChanged(), oldObj, newObj)).To(BeTrue())
			})
		})
	})

	// Config is a cluster-scoped singleton named "default", so these specs create and
	// remove it themselves and never run alongside other specs.
	Describe("TenantReconciler Config watch", Serial, func() {
		// The watch ANDs the singleton name filter with the change filter.
		configWatch := predicate.And(configResourceDefault(), configSpecOrDeletionChanged())
		var config *maasv1alpha1.Config

		BeforeEach(func(ctx SpecContext) {
			config = &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName}}
			Expect(envTest.Create(ctx, config)).To(Succeed())
			DeferCleanup(predRemoveConfig)
		})

		When("status is written", func() {
			It("does not admit the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, config, func(ctx context.Context, current *maasv1alpha1.Config) {
					apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
						Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionFalse, Reason: "OperandNotReady",
					})
					Expect(envTest.Status().Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				Expect(predAdmits(configWatch, oldObj, newObj)).To(BeFalse())
			})
		})

		When("spec.usageLogging changes", func() {
			It("admits the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, config, func(ctx context.Context, current *maasv1alpha1.Config) {
					current.Spec.UsageLogging = ptr.To(true)
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(BeNumerically(">", oldObj.Generation))
				Expect(predAdmits(configWatch, oldObj, newObj)).To(BeTrue())
			})
		})

		When("it is deleted while a finalizer holds it", func() {
			It("admits the update that sets deletionTimestamp", func(ctx SpecContext) {
				predAddFinalizer(ctx, config)

				oldObj, newObj := predWrite(ctx, config, func(ctx context.Context, current *maasv1alpha1.Config) {
					Expect(envTest.Delete(ctx, current)).To(Succeed())
				})

				Expect(newObj.DeletionTimestamp).NotTo(BeNil())
				Expect(predAdmits(configWatch, oldObj, newObj)).To(BeTrue())
			})
		})
	})

	Describe("TenantReconciler NetworkPolicy watch", func() {
		var policy *netwv1.NetworkPolicy

		BeforeEach(func(ctx SpecContext) {
			policy = predApplyNetworkPolicy(ctx, namespace, "tenant-a", "ai-tenant-tenant-a")
		})

		When("another tenant re-applies it with its own tracking labels", func() {
			It("admits the update: relabel suppression is gone", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, policy, func(ctx context.Context, _ *netwv1.NetworkPolicy) {
					predApplyNetworkPolicy(ctx, namespace, "tenant-b", "ai-tenant-tenant-b")
				})

				Expect(newObj.ResourceVersion).NotTo(Equal(oldObj.ResourceVersion))
				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				// A real shared NetworkPolicy never carries tracking labels post-render
				// (it gets the shared marker instead), so this relabel cannot happen
				// through the pipeline. The predicate no longer special-cases it: any
				// label change is drift.
				Expect(predAdmits(networkPolicyChangedForTenant(), oldObj, newObj)).To(BeTrue())
			})
		})

		When("a per-tenant NetworkPolicy is relabelled to another tenant", func() {
			It("admits the update and maps it back to its owner", func(ctx SpecContext) {
				red, blue := predTenantRequest("ai-tenant-red")[0], predTenantRequest("ai-tenant-blue")[0]
				perTenant := tenantNetworkPolicy(namespace, tenantreconcile.PayloadProcessingNetworkPolicyName("red"), "red", red.Namespace)
				Expect(applyNetworkPolicy(ctx, perTenant)).To(Succeed())

				oldObj, newObj := predWrite(ctx, perTenant, func(ctx context.Context, current *netwv1.NetworkPolicy) {
					current.Labels[tenantreconcile.LabelTenantName] = "blue"
					current.Labels[tenantreconcile.LabelTenantNamespace] = blue.Namespace
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				Expect(predAdmits(networkPolicyChangedForTenant(), oldObj, newObj)).To(BeTrue())
				// The mapper enqueues the old object's owner, the only tenant that renders it.
				r := &TenantReconciler{GatewayNamespace: namespace, TenantNamespaceDiscoveryEnabled: true}
				Expect(r.mapNetworkPolicyToMaasTenantConfigs(ctx, oldObj)).To(Equal([]reconcile.Request{red}))
			})
		})

		When("its spec changes", func() {
			It("admits the update", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, policy, func(ctx context.Context, current *netwv1.NetworkPolicy) {
					current.Spec.PodSelector.MatchLabels["app"] = "maas-api-v2"
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(BeNumerically(">", oldObj.Generation))
				Expect(predAdmits(networkPolicyChangedForTenant(), oldObj, newObj)).To(BeTrue())
			})
		})

		When("it is opted out with managed=false", func() {
			It("admits the annotation change", func(ctx SpecContext) {
				oldObj, newObj := predWrite(ctx, policy, func(ctx context.Context, current *netwv1.NetworkPolicy) {
					current.Annotations = map[string]string{tenantreconcile.AnnotationManaged: "false"}
					Expect(envTest.Update(ctx, current)).To(Succeed())
				})

				Expect(newObj.Generation).To(Equal(oldObj.Generation))
				Expect(predAdmits(networkPolicyChangedForTenant(), oldObj, newObj)).To(BeTrue())
			})
		})
	})

	DescribeTableSubtree("MaasTenantConfig readiness watch",
		func(readinessWatch predicate.Predicate) {
			var tenantConfig *maasv1alpha1.MaasTenantConfig

			BeforeEach(func(ctx SpecContext) {
				tenantConfig = predTenantConfig(namespace)
				Expect(envTest.Create(ctx, tenantConfig)).To(Succeed())
				predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
					current.Status.Phase = "Pending"
					apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
						Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionFalse,
						Reason: "DeploymentsNotReady", Message: "waiting for maas-api",
						ObservedGeneration: current.Generation,
					})
					Expect(envTest.Status().Update(ctx, current)).To(Succeed())
				})
			})

			When("Ready flips to True", func() {
				It("admits the update", func(ctx SpecContext) {
					oldObj, newObj := predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
						current.Status.Phase = "Active"
						apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
							Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionTrue,
							Reason: "Reconciled", ObservedGeneration: current.Generation,
						})
						Expect(envTest.Status().Update(ctx, current)).To(Succeed())
					})

					Expect(predAdmits(readinessWatch, oldObj, newObj)).To(BeTrue())
				})
			})

			When("unrelated status is written", func() {
				It("does not admit the update", func(ctx SpecContext) {
					oldObj, newObj := predWrite(ctx, tenantConfig, func(ctx context.Context, current *maasv1alpha1.MaasTenantConfig) {
						current.Status.InfraNamespace = "opendatahub"
						apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
							Type: tenantreconcile.ConditionTypeDegraded, Status: metav1.ConditionTrue,
							Reason: "PrerequisitesWarning", Message: "DSCI monitoring not configured",
						})
						Expect(envTest.Status().Update(ctx, current)).To(Succeed())
					})

					Expect(newObj.ResourceVersion).NotTo(Equal(oldObj.ResourceVersion))
					Expect(predAdmits(readinessWatch, oldObj, newObj)).To(BeFalse())
				})
			})
		},
		Entry("of the AITenantReconciler", tenantConfigChangedForAITenant()),
		Entry("of the LifecycleReconciler", tenantConfigChangedForLifecycle()),
	)
})

func predTenantConfig(namespace string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace},
	}
}

// predWrite reads obj, lets write change it through the API server and reads it again,
// returning both stored versions.
func predWrite[T client.Object](ctx context.Context, obj T, write func(context.Context, T)) (T, T) {
	GinkgoHelper()

	oldObj := predCopy(obj)
	Expect(envTest.Get(ctx, client.ObjectKeyFromObject(obj), oldObj)).To(Succeed())
	write(ctx, predCopy(oldObj))
	newObj := predCopy(obj)
	Expect(envTest.Get(ctx, client.ObjectKeyFromObject(obj), newObj)).To(Succeed())
	return oldObj, newObj
}

func predCopy[T client.Object](obj T) T {
	GinkgoHelper()

	out, ok := obj.DeepCopyObject().(T)
	Expect(ok).To(BeTrue())
	return out
}

func predAdmits(p predicate.Predicate, oldObj, newObj client.Object) bool {
	return p.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})
}

func predAddFinalizer[T client.Object](ctx context.Context, obj T) {
	GinkgoHelper()

	predWrite(ctx, obj, func(ctx context.Context, current T) {
		controllerutil.AddFinalizer(current, predHoldFinalizer)
		Expect(envTest.Update(ctx, current)).To(Succeed())
	})
}

// predRemoveConfig releases and deletes Config/default and waits until it is gone, so
// the next spec can create the singleton again.
func predRemoveConfig(ctx SpecContext) {
	config := &maasv1alpha1.Config{}
	key := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	Eventually(func(g Gomega) {
		err := envTest.Get(ctx, key, config)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		if controllerutil.RemoveFinalizer(config, predHoldFinalizer) {
			g.Expect(envTest.Update(ctx, config)).To(Succeed())
		}
		g.Expect(client.IgnoreNotFound(envTest.Delete(ctx, config))).To(Succeed())
		g.Expect(apierrors.IsNotFound(envTest.Get(ctx, key, config))).To(BeTrue())
	}).WithContext(ctx).Should(Succeed())
}

// predApplyNetworkPolicy server-side applies a NetworkPolicy named like the shared
// maas-api one but stamped with a tenant's tracking labels, a state the render pipeline
// no longer produces (a real shared object carries the shared marker instead) but that
// still probes the predicate's label-change handling directly.
func predApplyNetworkPolicy(ctx context.Context, namespace, tenantName, tenantNamespace string) *netwv1.NetworkPolicy {
	GinkgoHelper()

	policy := tenantNetworkPolicy(namespace, "maas-api", tenantName, tenantNamespace)
	Expect(applyNetworkPolicy(ctx, policy)).To(Succeed())
	return policy
}

func applyNetworkPolicy(ctx context.Context, policy *netwv1.NetworkPolicy) error {
	return envTest.Patch(ctx, policy, client.Apply, client.FieldOwner("maas-controller"), client.ForceOwnership)
}

// tenantNetworkPolicy is a NetworkPolicy as a tenant's platform apply renders it, stamped
// with that tenant's tracking labels.
func tenantNetworkPolicy(namespace, name, tenantName, tenantNamespace string) *netwv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	return &netwv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: netwv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":          "models-as-a-service",
				tenantreconcile.LabelTenantName:      tenantName,
				tenantreconcile.LabelTenantNamespace: tenantNamespace,
			},
		},
		Spec: netwv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-api"}},
			PolicyTypes: []netwv1.PolicyType{netwv1.PolicyTypeIngress},
			Ingress: []netwv1.NetworkPolicyIngressRule{{
				Ports: []netwv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(8443))}},
			}},
		},
	}
}

func predTenantRequest(tenantNamespace string) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: tenantNamespace,
	}}}
}
