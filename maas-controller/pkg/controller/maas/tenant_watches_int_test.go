package maas

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Watch settling window: the tenants count as settled once no request arrives for
// watchQuietPeriod, and a loop shows up as never settling within watchSettleTimeout.
const (
	watchQuietPeriod   = time.Second
	watchSettleTimeout = 15 * time.Second
)

// These specs run the watches SetupWithManager registers against a real API server, with
// a recorder in place of the platform reconcile, so a predicate that is not wired in or a
// write that re-enqueues its own tenant shows up as requests.
var _ = Describe("TenantReconciler watches", func() {
	var (
		appNamespace     string
		gatewayNamespace string
		tenantA          reconcile.Request
		tenantB          reconcile.Request
		recorder         *tenantRequestRecorder
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("app")).Name
		gatewayNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		tenantA = predTenantRequest(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant-a")).Name)[0]
		tenantB = predTenantRequest(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant-b")).Name)[0]
		for _, tenant := range []reconcile.Request{tenantA, tenantB} {
			Expect(envTest.Create(ctx, predTenantConfig(tenant.Namespace))).To(Succeed())
		}

		// Every platform pass applies the shared NetworkPolicy, marked shared like a real
		// render would, and its own per-tenant one stamped with its tracking labels. The
		// tenant namespace doubles as the tenant name. The shared policy's content never
		// varies by tenant, so every pass SSAs the same object: no relabel, no churn.
		recorder = newTenantRequestRecorder(func(ctx context.Context, tenant reconcile.Request) error {
			if tenant != tenantA && tenant != tenantB {
				return nil
			}
			perTenantName := tenantreconcile.PayloadProcessingNetworkPolicyName(tenant.Namespace)
			shared := tenantNetworkPolicy(appNamespace, "maas-api", tenant.Namespace, tenant.Namespace)
			delete(shared.Labels, tenantreconcile.LabelTenantName)
			delete(shared.Labels, tenantreconcile.LabelTenantNamespace)
			shared.Labels[tenantreconcile.LabelSharedOperand] = "true"
			return errors.Join(
				applyNetworkPolicy(ctx, shared),
				applyNetworkPolicy(ctx, tenantNetworkPolicy(gatewayNamespace, perTenantName, tenant.Namespace, tenant.Namespace)),
			)
		})
	})

	startWatches := func(ctx SpecContext) {
		GinkgoHelper()

		r := &TenantReconciler{
			AppNamespace:                    appNamespace,
			OperatorNamespace:               appNamespace,
			TenantNamespace:                 tenantA.Namespace,
			GatewayNamespace:                gatewayNamespace,
			TenantNamespaceDiscoveryEnabled: true,
		}
		startWatchManager(ctx, func(mgr ctrl.Manager) error {
			r.Client, r.Scheme = mgr.GetClient(), mgr.GetScheme()
			return r.setupWithManager(mgr, recorder)
		})

		By("letting both tenants apply the shared NetworkPolicy and settle")
		// The controller starts its workers once every watch has synced, which takes a few
		// seconds on the first manager in the process.
		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElements(tenantA, tenantB))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod),
			"tenants keep re-enqueueing each other")
	}

	It("settles because the shared NetworkPolicy carries the same marker for every tenant", func(ctx SpecContext) {
		startWatches(ctx)

		policy := &netwv1.NetworkPolicy{}
		Expect(envTest.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: "maas-api"}, policy)).To(Succeed())
		Expect(policy.Labels).To(HaveKeyWithValue(tenantreconcile.LabelSharedOperand, "true"))
		Expect(policy.Labels).NotTo(HaveKey(tenantreconcile.LabelTenantNamespace))
	})

	It("settles when the tenants apply the rendered cleanup NetworkPolicy", func(ctx SpecContext) {
		// The rendered manifest goes to server-side apply unstructured, as the platform
		// apply sends it, so an empty list in it reaches the API server. It is shared
		// (never renamed), so every pass marks it instead of stamping tracking labels.
		cleanupPolicy := renderedOperand(appNamespace, tenantreconcile.GVKNetworkPolicy, "maas-api-cleanup-restrict")
		recorder = newTenantRequestRecorder(func(ctx context.Context, tenant reconcile.Request) error {
			if tenant != tenantA && tenant != tenantB {
				return nil
			}
			policy := cleanupPolicy.DeepCopy()
			labels := policy.GetLabels()
			labels[tenantreconcile.LabelSharedOperand] = "true"
			policy.SetLabels(labels)
			return envTest.Patch(ctx, policy, client.Apply, client.FieldOwner("maas-controller"), client.ForceOwnership)
		})

		startWatches(ctx)
	})

	It("enqueues one tenant to repair the shared NetworkPolicy when it drifts", func(ctx SpecContext) {
		// Both tenants would repair it: tenantB is AITenant-managed and both are Ready.
		Eventually(func(g Gomega) {
			tenantConfig := &maasv1alpha1.MaasTenantConfig{}
			g.Expect(envTest.Get(ctx, tenantB.NamespacedName, tenantConfig)).To(Succeed())
			tenantConfig.Labels = aiTenantConfig(tenantB.Namespace, "tenant-b").Labels
			g.Expect(envTest.Update(ctx, tenantConfig)).To(Succeed())
		}).Should(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(envTest.Delete(ctx, predTenantConfig(tenantB.Namespace)))).To(Succeed())
		})
		for _, tenant := range []reconcile.Request{tenantA, tenantB} {
			setTenantReady(ctx, tenant.NamespacedName, metav1.ConditionTrue)
		}
		startWatches(ctx)
		mark := recorder.mark()

		policy := &netwv1.NetworkPolicy{}
		Expect(envTest.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: "maas-api"}, policy)).To(Succeed())
		policy.Spec.PodSelector.MatchLabels["app"] = "edited"
		Expect(envTest.Update(ctx, policy)).To(Succeed())

		// tenantA is the Ready default tenant (TenantNamespace) in startWatches, so it
		// repairs the shared policy alone: every tenant renders it identically, and fanning
		// the drift out to tenantB too would cost it a pass for nothing.
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(tenantA))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		Expect(recorder.since(mark)).NotTo(ContainElement(tenantB))
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		Expect(policy.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", "maas-api"))
	})

	It("restores a per-tenant NetworkPolicy relabelled to another tenant, then deleted", func(ctx SpecContext) {
		startWatches(ctx)
		policy := &netwv1.NetworkPolicy{}
		key := client.ObjectKey{Namespace: gatewayNamespace, Name: tenantreconcile.PayloadProcessingNetworkPolicyName(tenantA.Namespace)}
		trackedTenant := func(g Gomega) string {
			current := &netwv1.NetworkPolicy{}
			g.Expect(envTest.Get(ctx, key, current)).To(Succeed())
			return current.Labels[tenantreconcile.LabelTenantNamespace]
		}

		By("relabelling tenant A's policy to tenant B")
		Expect(envTest.Get(ctx, key, policy)).To(Succeed())
		policy.Labels[tenantreconcile.LabelTenantName] = tenantB.Namespace
		policy.Labels[tenantreconcile.LabelTenantNamespace] = tenantB.Namespace
		Expect(envTest.Update(ctx, policy)).To(Succeed())
		Eventually(trackedTenant).Should(Equal(tenantA.Namespace))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod),
			"the owner's restamp bounces between the tenants")

		By("relabelling it again and deleting it before its owner restores it")
		mark := recorder.mark()
		Expect(envTest.Get(ctx, key, policy)).To(Succeed())
		policy.Labels[tenantreconcile.LabelTenantName] = tenantB.Namespace
		policy.Labels[tenantreconcile.LabelTenantNamespace] = tenantB.Namespace
		Expect(envTest.Update(ctx, policy)).To(Succeed())
		Expect(client.IgnoreNotFound(envTest.Delete(ctx, policy))).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(tenantA))
		Eventually(trackedTenant).Should(Equal(tenantA.Namespace))
	})

	It("enqueues every tenant when maas-db-config changes", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		Expect(envTest.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: appNamespace},
		})).To(Succeed())

		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
	})

	It("enqueues the tenant when its AITenant's gateway changes, not on other status", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		aitenant := &maasv1alpha1.AITenant{ObjectMeta: metav1.ObjectMeta{Name: "team-watch", Namespace: appNamespace}}
		owned := predTenantRequest(tenantreconcile.TenantNamespaceForAITenant(aitenant.Name, tenantA.Namespace))[0]
		Expect(envTest.Create(ctx, aitenant)).To(Succeed())
		// The first status write resolves the gateway, as the AITenant controller does; the
		// CRD defaults status.gatewayRef, so no later write leaves it empty.
		aitenant.Status.Phase = "Pending"
		aitenant.Status.GatewayRef = maasv1alpha1.TenantGatewayRef{Name: "team-watch", Namespace: "openshift-ingress"}
		Expect(envTest.Status().Update(ctx, aitenant)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(owned))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark = recorder.mark()
		aitenant.Status.Phase = "Active"
		Expect(envTest.Status().Update(ctx, aitenant)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(owned))

		aitenant.Status.GatewayRef.Name = "team-watch-v2"
		Expect(envTest.Status().Update(ctx, aitenant)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(owned))
	})

	// Config is a cluster-scoped singleton, so this spec never runs alongside the others
	// that create it.
	It("drops Config status writes and fans spec changes out to every tenant", Serial, func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()
		config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName}}
		Expect(envTest.Create(ctx, config)).To(Succeed())
		DeferCleanup(predRemoveConfig)
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark = recorder.mark()
		apimeta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionFalse, Reason: "OperandNotReady",
		})
		Expect(envTest.Status().Update(ctx, config)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(tenantA))

		config.Spec.UsageLogging = ptr.To(true)
		Expect(envTest.Update(ctx, config)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
	})

	It("drops DSCInitialization spec edits and fans monitoring changes out to every tenant", func(ctx SpecContext) {
		installSchemalessCRD(dsciGVK, "dscinitializations", extv1.ClusterScoped)
		startWatches(ctx)
		mark := recorder.mark()

		dsci := &unstructured.Unstructured{}
		dsci.SetGroupVersionKind(dsciGVK)
		dsci.SetName(appNamespace)
		Expect(envTest.Create(ctx, dsci)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))

		mark = recorder.mark()
		Expect(unstructured.SetNestedField(dsci.Object, "opendatahub", "spec", "applicationsNamespace")).To(Succeed())
		Expect(envTest.Update(ctx, dsci)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(tenantA))

		conditions := []any{map[string]any{
			"type": "MonitoringReady", "status": "False", "reason": "Deploying", "message": "monitoring stack starting",
			"lastTransitionTime": "2026-09-30T00:00:00Z",
		}}
		Expect(unstructured.SetNestedSlice(dsci.Object, conditions, "status", "conditions")).To(Succeed())
		Expect(envTest.Status().Update(ctx, dsci)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
	})

	It("drops MaasTenantConfig status writes but not annotation changes", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		tenantConfig := &maasv1alpha1.MaasTenantConfig{}
		Expect(envTest.Get(ctx, tenantA.NamespacedName, tenantConfig)).To(Succeed())
		tenantConfig.Status.Phase = "Active"
		Expect(envTest.Status().Update(ctx, tenantConfig)).To(Succeed())
		Consistently(func() []reconcile.Request { return recorder.since(mark) }).WithTimeout(watchQuietPeriod).
			ShouldNot(ContainElement(tenantA))

		tenantConfig.Annotations = map[string]string{managementStateAnnotation: managementStateManaged}
		Expect(envTest.Update(ctx, tenantConfig)).To(Succeed())
		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(tenantA))
	})
})

var _ = Describe("AITenantReconciler watches", func() {
	It("re-enqueues the AITenant when its MaasTenantConfig reports Ready", func(ctx SpecContext) {
		installSchemalessCRD(gatewayGVK, "gateways", extv1.NamespaceScoped)
		aitenant := reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      "team-a",
			Namespace: pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("infra")).Name,
		}}
		recorder := newTenantRequestRecorder(func(context.Context, reconcile.Request) error { return nil })
		r := &AITenantReconciler{}
		startWatchManager(ctx, func(mgr ctrl.Manager) error { return r.setupWithManager(mgr, recorder) })

		tenantConfig := predTenantConfig(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant")).Name)
		tenantConfig.Annotations = map[string]string{
			aitenantNameAnnotation:      aitenant.Name,
			aitenantNamespaceAnnotation: aitenant.Namespace,
		}
		Expect(envTest.Create(ctx, tenantConfig)).To(Succeed())
		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElement(aitenant))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		mark := recorder.mark()

		apimeta.SetStatusCondition(&tenantConfig.Status.Conditions, metav1.Condition{
			Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionTrue,
			Reason: "Reconciled", ObservedGeneration: tenantConfig.Generation,
		})
		Expect(envTest.Status().Update(ctx, tenantConfig)).To(Succeed())

		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElement(aitenant))
	})
})

// renderedOperand renders the odh overlay into appNamespace the way the platform pipeline
// does and returns the named object.
func renderedOperand(appNamespace string, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	GinkgoHelper()

	overlay := filepath.Join(pkgtest.ProjectRoot(), "maas-api", "deploy", "overlays", "odh")
	rendered, err := tenantreconcile.RenderKustomize(overlay, appNamespace)
	Expect(err).NotTo(HaveOccurred())
	for i := range rendered {
		if rendered[i].GroupVersionKind() == gvk && rendered[i].GetName() == name {
			return &rendered[i]
		}
	}
	Fail(fmt.Sprintf("the odh overlay renders no %s %q", gvk.Kind, name))
	return nil
}

// installSchemalessCRD serves gvk without a schema and with a status subresource, for
// watches on APIs the environment does not install.
func installSchemalessCRD(gvk schema.GroupVersionKind, plural string, scope extv1.ResourceScope) {
	GinkgoHelper()

	crd := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + gvk.Group},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: gvk.Group,
			Names: extv1.CustomResourceDefinitionNames{
				Plural: plural, Singular: strings.ToLower(gvk.Kind), Kind: gvk.Kind, ListKind: gvk.Kind + "List",
			},
			Scope: scope,
			Versions: []extv1.CustomResourceDefinitionVersion{{
				Name: gvk.Version, Served: true, Storage: true,
				Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true),
				}},
				Subresources: &extv1.CustomResourceSubresources{Status: &extv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
	if strings.HasSuffix(gvk.Group, ".k8s.io") {
		// The API server protects *.k8s.io groups.
		crd.Annotations = map[string]string{"api-approved.kubernetes.io": "unapproved, served only to envtest specs"}
	}
	_, err := envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{CRDs: []*extv1.CustomResourceDefinition{crd}})
	Expect(err).NotTo(HaveOccurred())
}

// startWatchManager runs a manager with the watches setup registers until the spec ends.
func startWatchManager(ctx SpecContext, setup func(ctrl.Manager) error) {
	GinkgoHelper()

	mgr, err := ctrl.NewManager(envTest.Config, ctrl.Options{
		Scheme:                 watchScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Every spec builds the same controller in a fresh manager.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(setup(mgr)).To(Succeed())

	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
	DeferCleanup(func() { Eventually(stopped).Should(BeClosed()) })
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
}

func watchScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	utilruntime.Must(extv1.AddToScheme(s))
	utilruntime.Must(gatewayapiv1.Install(s))
	return s
}

// tenantRequestRecorder stands in for the platform reconcile. It records every request
// the watches enqueue and runs pass for it, returning its error.
type tenantRequestRecorder struct {
	pass func(context.Context, reconcile.Request) error

	mu       sync.Mutex
	requests []reconcile.Request
	last     time.Time
}

func newTenantRequestRecorder(pass func(context.Context, reconcile.Request) error) *tenantRequestRecorder {
	return &tenantRequestRecorder{pass: pass}
}

func (r *tenantRequestRecorder) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.last = time.Now()
	r.mu.Unlock()
	return reconcile.Result{}, r.pass(ctx, req)
}

func (r *tenantRequestRecorder) all() []reconcile.Request {
	return r.since(0)
}

// mark returns a position that since reads from.
func (r *tenantRequestRecorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *tenantRequestRecorder) since(mark int) []reconcile.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reconcile.Request(nil), r.requests[mark:]...)
}

// idleFor reports how long ago the last request arrived.
func (r *tenantRequestRecorder) idleFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.last)
}
