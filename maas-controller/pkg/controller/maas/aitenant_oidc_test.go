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
// reconcile throttle: the same (issuer, generation) is not re-probed within
// the throttle window, and a cached result is reused.
func TestReconcileOIDCReady_ThrottlesRepeatedProbes(t *testing.T) {
	g := NewWithT(t)

	aitenant := oidcTestAITenant()
	r, _ := oidcTestReconciler(t, aitenant)
	probe, calls := fakeProbe(oidcprobe.Result{Reason: oidcprobe.ReasonIssuerUnreachable, Message: "connection refused"})
	r.OIDCProbe = probe

	r.reconcileOIDCReady(context.Background(), aitenant)
	r.reconcileOIDCReady(context.Background(), aitenant)
	r.reconcileOIDCReady(context.Background(), aitenant)

	g.Expect(*calls).To(Equal(1), "repeated reconciles within the throttle window must reuse the cached probe")
	cond := apimeta.FindStatusCondition(aitenant.Status.Conditions, maasv1alpha1.AITenantConditionOIDCReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(oidcprobe.ReasonIssuerUnreachable))

	// A generation change must trigger a fresh probe.
	aitenant.Generation = 2
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(*calls).To(Equal(2))

	// A recreated AITenant (same name, new UID) must not inherit the old
	// object's cached probe result.
	aitenant.Generation = 1
	aitenant.UID = "a-new-uid"
	r.reconcileOIDCReady(context.Background(), aitenant)
	g.Expect(*calls).To(Equal(3))
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
