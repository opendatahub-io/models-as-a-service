package maas

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
		appNamespace string
		tenantA      reconcile.Request
		tenantB      reconcile.Request
		recorder     *tenantRequestRecorder
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("app")).Name
		tenantA = predTenantRequest(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant-a")).Name)[0]
		tenantB = predTenantRequest(pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("tenant-b")).Name)[0]
		for _, tenant := range []reconcile.Request{tenantA, tenantB} {
			Expect(envTest.Create(ctx, predTenantConfig(tenant.Namespace))).To(Succeed())
		}

		// Every platform pass applies the shared NetworkPolicies stamped with its own tenant.
		recorder = newTenantRequestRecorder(func(ctx context.Context, tenant reconcile.Request) error {
			if tenant != tenantA && tenant != tenantB {
				return nil
			}
			return applySharedNetworkPolicy(ctx, sharedNetworkPolicy(appNamespace, tenant.Namespace, tenant.Namespace))
		})
	})

	startWatches := func(ctx SpecContext) {
		GinkgoHelper()

		r := &TenantReconciler{
			AppNamespace:                    appNamespace,
			OperatorNamespace:               appNamespace,
			TenantNamespace:                 tenantA.Namespace,
			GatewayNamespace:                "openshift-ingress",
			TenantNamespaceDiscoveryEnabled: true,
		}
		startTenantWatches(ctx, r, recorder)

		By("letting both tenants apply the shared NetworkPolicy and settle")
		// The controller starts its workers once every watch has synced, which takes a few
		// seconds on the first manager in the process.
		Eventually(recorder.all).WithTimeout(watchSettleTimeout).Should(ContainElements(tenantA, tenantB))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod),
			"tenants keep re-enqueueing each other")
	}

	It("settles after the tenants restamp each other's labels on the shared NetworkPolicy", func(ctx SpecContext) {
		startWatches(ctx)

		policy := &netwv1.NetworkPolicy{}
		Expect(envTest.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: "maas-api"}, policy)).To(Succeed())
		Expect(policy.Labels).To(HaveKey(tenantreconcile.LabelTenantNamespace))
	})

	It("enqueues every tenant when the shared NetworkPolicy drifts", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		policy := &netwv1.NetworkPolicy{}
		Expect(envTest.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: "maas-api"}, policy)).To(Succeed())
		policy.Spec.PodSelector.MatchLabels["app"] = "edited"
		Expect(envTest.Update(ctx, policy)).To(Succeed())

		Eventually(func() []reconcile.Request { return recorder.since(mark) }).Should(ContainElements(tenantA, tenantB))
		Eventually(recorder.idleFor).WithTimeout(watchSettleTimeout).Should(BeNumerically(">=", watchQuietPeriod))
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(policy), policy)).To(Succeed())
		Expect(policy.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app", "maas-api"))
	})

	It("enqueues every tenant when maas-db-config changes", func(ctx SpecContext) {
		startWatches(ctx)
		mark := recorder.mark()

		Expect(envTest.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: appNamespace},
		})).To(Succeed())

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

// startTenantWatches runs a manager with r's watches feeding target until the spec ends.
func startTenantWatches(ctx SpecContext, r *TenantReconciler, target reconcile.Reconciler) {
	GinkgoHelper()

	mgr, err := ctrl.NewManager(envTest.Config, ctrl.Options{
		Scheme:                 tenantWatchScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Every spec builds the same controller in a fresh manager.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	Expect(r.setupWithManager(mgr, target)).To(Succeed())

	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
	DeferCleanup(func() { Eventually(stopped).Should(BeClosed()) })
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
}

func tenantWatchScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	utilruntime.Must(extv1.AddToScheme(s))
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
