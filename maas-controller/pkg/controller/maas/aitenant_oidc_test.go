//nolint:testpackage
package maas

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oidcprobe"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func oidcTestReconciler(t *testing.T, objects ...client.Object) (*AITenantReconciler, client.WithWatch) {
	t.Helper()
	s := aitenantTestScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(objects...).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}
	return r, cl
}

func oidcTestAITenant() *maasv1alpha1.AITenant {
	return &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-oidc",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/team-oidc",
				ClientID:  "team-oidc-client",
			},
		},
	}
}

func drainEvents(recorder *events.FakeRecorder) []string {
	var collected []string
	for {
		select {
		case e := <-recorder.Events:
			collected = append(collected, e)
		default:
			return collected
		}
	}
}

func fakeProbe(result oidcprobe.Result) (oidcProbeFunc, *int) {
	calls := 0
	return func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		calls++
		return result
	}, &calls
}

// TestReconcileOIDCReady_ProbeOK sets a True/Available condition with the
// observed generation and leaves Ready alone.
func TestReconcileOIDCReady_ProbeOK(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	aitenant.Generation = 7
	r, _ := oidcTestReconciler(t, aitenant)
	probe, calls := fakeProbe(oidcprobe.Result{OK: true, Reason: oidcprobe.ReasonAvailable, Message: "issuer reachable"})
	r.OIDCProbe = probe

	r.reconcileOIDCReady(context.Background(), aitenant)

	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonAvailable))
	g.Expect(cond.ObservedGeneration).To(Equal(int64(7)))
	g.Expect(*calls).To(Equal(1))
	// Ready is independent of OIDC health and must not be touched.
	g.Expect(apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionReady)).To(BeNil())
}

// TestReconcileOIDCReady_ProbeFailure reports the failure reason without
// touching Ready or the phase.
func TestReconcileOIDCReady_ProbeFailure(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	aitenant.Status.Phase = "Active"
	r, _ := oidcTestReconciler(t, aitenant)
	r.OIDCProbe = func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		return oidcprobe.Result{Reason: oidcprobe.ReasonTLSUntrusted, Message: "certificate signed by unknown authority"}
	}

	r.reconcileOIDCReady(context.Background(), aitenant)

	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonTLSUntrusted))
	g.Expect(aitenant.Status.Phase).To(Equal("Active"))
	g.Expect(apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionReady)).To(BeNil())
}

// TestReconcileOIDCReady_RemovesConditionWhenOIDCUnset drops the condition
// when spec.oidc or issuerUrl is removed.
func TestReconcileOIDCReady_RemovesConditionWhenOIDCUnset(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	aitenant.Spec.OIDC = nil
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AITenantConditionOIDCReady,
		Status:             metav1.ConditionTrue,
		Reason:             oidcprobe.ReasonAvailable,
		ObservedGeneration: 1,
	})
	r, _ := oidcTestReconciler(t, aitenant)
	r.OIDCProbe = func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		t.Fatal("probe must not run when OIDC is not configured")
		return oidcprobe.Result{}
	}

	r.reconcileOIDCReady(context.Background(), aitenant)

	g.Expect(apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)).To(BeNil())
}

// TestReconcileOIDCReady_ClientSecretRefMissingKey reports
// ClientConfigIncomplete without probing the issuer.
func TestReconcileOIDCReady_ClientSecretRefMissingKey(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	aitenant.Spec.OIDC.ClientSecretRef = &maasv1alpha1.OIDCClientSecretReference{Name: "portal-client", Key: "client-secret"}
	tenantNS := tenantreconcile.TenantNamespaceForAITenant(aitenant.Name, "models-as-a-service")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "portal-client", Namespace: tenantNS},
		Data:       map[string][]byte{"other-key": []byte("value")},
	}
	r, _ := oidcTestReconciler(t, aitenant, secret)
	r.OIDCProbe = func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		t.Fatal("probe must not run when the client config is incomplete")
		return oidcprobe.Result{}
	}

	r.reconcileOIDCReady(context.Background(), aitenant)

	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonClientConfigIncomplete))
	g.Expect(cond.Message).To(ContainSubstring("client-secret"))
}

// TestReconcileOIDCReady_ClientSecretRefMissingSecret reports
// ClientConfigIncomplete when the referenced Secret does not exist.
func TestReconcileOIDCReady_ClientSecretRefMissingSecret(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	aitenant.Spec.OIDC.ClientSecretRef = &maasv1alpha1.OIDCClientSecretReference{Name: "missing-secret", Key: "client-secret"}
	r, _ := oidcTestReconciler(t, aitenant)

	r.reconcileOIDCReady(context.Background(), aitenant)

	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonClientConfigIncomplete))
}

// TestReconcileOIDCReady_CABundlePassedToProbe verifies the trusted CA
// ConfigMap from the infra namespace lands in the probe's root pool.
func TestReconcileOIDCReady_CABundlePassedToProbe(t *testing.T) {
	g := NewWithT(t)

	// Generate a CA and a leaf signed by it; the probe's pool must verify the
	// leaf, proving the ConfigMap PEM was appended to the system roots.
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	g.Expect(err).NotTo(HaveOccurred())
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "maas-oidc-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	g.Expect(err).NotTo(HaveOccurred())
	caCert, err := x509.ParseCertificate(caDER)
	g.Expect(err).NotTo(HaveOccurred())

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	g.Expect(err).NotTo(HaveOccurred())
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "probe-test"},
		DNSNames:     []string{"probe-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	g.Expect(err).NotTo(HaveOccurred())
	leafCert, err := x509.ParseCertificate(leafDER)
	g.Expect(err).NotTo(HaveOccurred())

	aitenant := oidcTestAITenant()
	caBundle := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: oidcTrustedCAConfigMapName, Namespace: "opendatahub"},
		Data: map[string]string{
			"ca-bundle.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		},
	}
	r, _ := oidcTestReconciler(t, aitenant, caBundle)

	var captured *x509.CertPool
	r.OIDCProbe = func(_ context.Context, _ string, roots *x509.CertPool) oidcprobe.Result {
		captured = roots
		return oidcprobe.Result{OK: true, Reason: oidcprobe.ReasonAvailable}
	}

	r.reconcileOIDCReady(context.Background(), aitenant)

	g.Expect(captured).NotTo(BeNil())
	_, err = leafCert.Verify(x509.VerifyOptions{Roots: captured, DNSName: "probe-test"})
	g.Expect(err).NotTo(HaveOccurred(), "probe root pool must contain the infra-namespace CA bundle")
}

// TestReconcileOIDCReady_ThrottlesRepeatedProbes verifies the watch-driven
// reconcile throttle: the cache is keyed by issuer (not tenant), so repeated
// reconciles and tenants sharing an issuer reuse one probe within the window,
// while a different issuer gets a fresh probe.
func TestReconcileOIDCReady_ThrottlesRepeatedProbes(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	other := oidcTestAITenant()
	other.Name = "team-other"
	r, _ := oidcTestReconciler(t, aitenant, other)
	probe, calls := fakeProbe(oidcprobe.Result{Reason: oidcprobe.ReasonIssuerUnreachable, Message: "unreachable"})
	r.OIDCProbe = probe

	// Repeated reconciles of the same tenant reuse the cached probe.
	r.reconcileOIDCReady(context.Background(), aitenant)
	r.reconcileOIDCReady(context.Background(), aitenant)
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(*calls).To(Equal(1), "repeated reconciles within the throttle window must reuse the cached probe")
	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonIssuerUnreachable))

	// A tenant sharing the same issuer (trailing slash included) reuses it
	// too: one dead IdP must not multiply into one probe per tenant.
	other.Spec.OIDC.IssuerURL = aitenant.Spec.OIDC.IssuerURL + "/"
	r.reconcileOIDCReady(context.Background(), other)
	g.Expect(*calls).To(Equal(1), "a shared issuer must share the cached probe")

	// A different issuer gets a fresh probe.
	aitenant.Spec.OIDC.IssuerURL = "https://other-issuer.example.com/realms/x"
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(*calls).To(Equal(2))
}

// TestAITenantReconcile_OIDCRequeueAndReadyUnchanged runs the full reconcile
// with a failing probe: the AITenant stays Active with Ready=True while
// OIDCReady=False, and the happy path requeues after oidcRequeueInterval.
func TestAITenantReconcile_OIDCRequeueAndReadyUnchanged(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := oidcTestAITenant()
	aitenant.Name = "team-a"
	gateway := existingAITenantGateway("team-a")
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, gateway).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
		OIDCProbe: func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
			return oidcprobe.Result{Reason: oidcprobe.ReasonIssuerUnreachable, Message: "connection refused"}
		},
	}

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	res := reconcileAITenantWithReadyTenantConfig(t, r, key)

	g.Expect(res.RequeueAfter).To(Equal(oidcRequeueInterval), "happy path with OIDC must requeue to re-probe the issuer")

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "OIDC failure must not change Ready")
	oidcReady := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(oidcReady).NotTo(BeNil())
	g.Expect(oidcReady.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(oidcReady.Reason).To(Equal(oidcprobe.ReasonIssuerUnreachable))
}

// TestAITenantReconcile_NoOIDCNoRequeue verifies the happy path keeps
// returning no requeue when OIDC is not configured.
func TestAITenantReconcile_NoOIDCNoRequeue(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: tenantreconcile.DefaultAITenantNamespace},
	}
	gateway := existingAITenantGateway("team-a")
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, gateway).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}
	r.OIDCProbe = func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		t.Fatal("probe must not run when OIDC is not configured")
		return oidcprobe.Result{}
	}

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	res := reconcileAITenantWithReadyTenantConfig(t, r, key)
	g.Expect(res).To(Equal(ctrl.Result{}))
}

// TestReconcileOIDCReady_WarningEventOnTransitionToFalse verifies a warning
// event is emitted exactly once, when the condition first turns False — not
// on every re-probe while it stays False.
func TestReconcileOIDCReady_WarningEventOnTransitionToFalse(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	r, _ := oidcTestReconciler(t, aitenant)
	recorder := events.NewFakeRecorder(10)
	r.Recorder = recorder
	r.OIDCProbe = func(_ context.Context, _ string, _ *x509.CertPool) oidcprobe.Result {
		return oidcprobe.Result{Reason: oidcprobe.ReasonIssuerUnreachable, Message: "unreachable"}
	}

	r.reconcileOIDCReady(context.Background(), aitenant)
	emitted := drainEvents(recorder)
	g.Expect(emitted).To(HaveLen(1), "transition to False must emit exactly one warning event")
	g.Expect(emitted[0]).To(ContainSubstring("Warning"))
	g.Expect(emitted[0]).To(ContainSubstring("OIDCNotReady"))
	g.Expect(emitted[0]).To(ContainSubstring(oidcprobe.ReasonIssuerUnreachable))

	// Staying False must not re-emit — with a fresh probe, not a cache hit:
	// the second call uses a different issuer so the probe runs again and
	// only the previous-condition check suppresses the event.
	aitenant.Spec.OIDC.IssuerURL = "https://other-issuer.example.com/realms/x"
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(drainEvents(recorder)).To(BeEmpty(), "a steady False condition must not spam events")

	// The cache-hit path must not re-emit either.
	aitenant.Spec.OIDC.IssuerURL = "https://issuer.example.com/realms/team-oidc"
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(drainEvents(recorder)).To(BeEmpty(), "a cached probe must not re-emit the event")
}

// TestReconcileOIDCReady_ConditionRemovedOnFailedTenant verifies the early
// Failed paths clear a stale OIDCReady: removing spec.oidc on a tenant that
// cannot reconcile must not leave the old condition behind.
func TestReconcileOIDCReady_ConditionRemovedOnFailedTenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	// Placement-invalid tenant carrying a stale condition from before
	// spec.oidc was removed.
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-wrong-infra", Namespace: "other-infra"},
	}
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AITenantConditionOIDCReady,
		Status:             metav1.ConditionTrue,
		Reason:             oidcprobe.ReasonAvailable,
		ObservedGeneration: 1,
	})
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	reconcileAITenantTwice(t, r, key)

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	g.Expect(apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)).To(BeNil(),
		"a failed tenant without spec.oidc must not keep a stale OIDCReady condition")
}

// TestUpdateAITenantStatusPreservesForeignConditions verifies the per-
// condition ownership contract: a full-status write must keep conditions
// owned by other controllers (for example the consumer portal's
// PortalProxyReady from ai-gateway-controller), both when the snapshot does
// not know them and when it carries a stale copy.
func TestUpdateAITenantStatusPreservesForeignConditions(t *testing.T) {
	g := NewWithT(t)

	// The live object carries a fresh foreign condition.
	live := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-oidc", Namespace: tenantreconcile.DefaultAITenantNamespace},
		Status: maasv1alpha1.AITenantStatus{
			Phase: "Active",
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled"},
				{Type: "PortalProxyReady", Status: metav1.ConditionFalse, Reason: "CrashLoopBackOff", Message: "fresh"},
			},
		},
	}
	// The reconciler's in-memory object is a stale read: its own condition
	// changed, and it carries an outdated foreign condition.
	aitenant := live.DeepCopy()
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "GatewayCheckFailed",
	})
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type: "PortalProxyReady", Status: metav1.ConditionTrue, Reason: "Available", Message: "stale",
	})
	snapshot := aitenant.Status.DeepCopy()
	snapshot.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled"}}

	r, _ := oidcTestReconciler(t, live)
	g.Expect(r.updateAITenantStatus(context.Background(), aitenant, snapshot)).To(Succeed())

	var updated maasv1alpha1.AITenant
	g.Expect(r.Get(context.Background(), client.ObjectKeyFromObject(live), &updated)).To(Succeed())

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("GatewayCheckFailed"), "owned condition must take the fresh value")

	proxy := apimeta.FindStatusCondition(updated.Status.Conditions, "PortalProxyReady")
	g.Expect(proxy).NotTo(BeNil())
	g.Expect(proxy.Message).To(Equal("fresh"), "foreign condition must keep the live value, not the stale snapshot copy")
}
