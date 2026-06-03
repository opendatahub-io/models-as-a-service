//nolint:testpackage
package maas

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func aitenantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(gatewayapiv1.Install(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func existingAITenantGateway(name string) *gatewayapiv1.Gateway {
	return &gatewayapiv1.Gateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gatewayapiv1.GroupVersion.String(),
			Kind:       "Gateway",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-ingress",
			Labels: map[string]string{
				"platform.opendatahub.io/owner": "gateway-controller",
			},
		},
		Spec: gatewayapiv1.GatewaySpec{
			GatewayClassName: gatewayapiv1.ObjectName("openshift-default"),
		},
	}
}

func ownedAITenantGateway(name, aitenantName, aitenantNamespace string) *gatewayapiv1.Gateway {
	gateway := existingAITenantGateway(name)
	gateway.Annotations = map[string]string{
		aitenantNameAnnotation:      aitenantName,
		aitenantNamespaceAnnotation: aitenantNamespace,
	}
	return gateway
}

func createdAITenantGateway(name, aitenantName, aitenantNamespace string) *gatewayapiv1.Gateway {
	gateway := ownedAITenantGateway(name, aitenantName, aitenantNamespace)
	gateway.Annotations[aitenantCreatedAnnotation] = "true"
	return gateway
}

type firstNotFoundReader struct {
	client.Reader
	first    bool
	resource schema.GroupResource
}

func (r *firstNotFoundReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.first {
		r.first = false
		return apierrors.NewNotFound(r.resource, key.Name)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func reconcileAITenantTwice(t *testing.T, r *AITenantReconciler, key types.NamespacedName) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(res).To(Equal(ctrl.Result{}))
}

func TestAITenantReconcile_CreatesBootstrapResources(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-a-maas"},
			Gateway: &maasv1alpha1.AITenantGatewayRef{
				GatewayClassName: "openshift-default",
			},
			Domain: "apps.example.com",
			TLS: &maasv1alpha1.AITenantTLSConfig{
				CertificateRef: maasv1alpha1.AITenantTLSCertificateRef{
					Name:      "team-a-cert",
					Namespace: "ai-tenants",
				},
			},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/team-a",
				ClientID:  "team-a-client",
			},
			RBAC: &maasv1alpha1.AITenantRBACConfig{
				Admins: []maasv1alpha1.AITenantRBACSubject{{
					Kind: rbacv1.GroupKind,
					Name: "team-a-admins",
				}},
			},
		},
	}
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

	var ns corev1.Namespace
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-a-maas"}, &ns)).To(Succeed())
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aitenantCreatedAnnotation, "true"))
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("opendatahub.io/generated-namespace", "true"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(aitenantManagedLabel, "true"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-name", "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-namespace", "team-a-maas"))

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-a", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Spec.GatewayClassName).To(Equal(gatewayapiv1.ObjectName("openshift-default")))
	g.Expect(gateway.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-a"))
	g.Expect(gateway.Labels).To(HaveKeyWithValue(aitenantManagedLabel, "true"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-a"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantNamespaceAnnotation, "ai-tenants"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantCreatedAnnotation, "true"))
	g.Expect(gateway.Spec.Listeners).To(HaveLen(2))
	g.Expect(gateway.Spec.Listeners[0].Name).To(Equal(gatewayapiv1.SectionName("http")))
	g.Expect(gateway.Spec.Listeners[0].Hostname).NotTo(BeNil())
	g.Expect(*gateway.Spec.Listeners[0].Hostname).To(Equal(gatewayapiv1.Hostname("team-a.apps.example.com")))
	g.Expect(gateway.Spec.Listeners[0].AllowedRoutes.Namespaces.From).NotTo(BeNil())
	g.Expect(*gateway.Spec.Listeners[0].AllowedRoutes.Namespaces.From).To(Equal(gatewayapiv1.NamespacesFromAll))
	g.Expect(gateway.Spec.Listeners[1].Name).To(Equal(gatewayapiv1.SectionName("https")))
	g.Expect(gateway.Spec.Listeners[1].TLS).NotTo(BeNil())
	g.Expect(gateway.Spec.Listeners[1].TLS.CertificateRefs).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[1].TLS.CertificateRefs[0].Name).To(Equal(gatewayapiv1.ObjectName("team-a-cert")))
	g.Expect(gateway.Spec.Listeners[1].TLS.CertificateRefs[0].Namespace).NotTo(BeNil())
	g.Expect(*gateway.Spec.Listeners[1].TLS.CertificateRefs[0].Namespace).To(Equal(gatewayapiv1.Namespace("ai-tenants")))

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-a-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "team-a",
	}))
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://issuer.example.com/realms/team-a"))
	g.Expect(tenant.Spec.ExternalOIDC.ClientID).To(Equal("team-a-client"))
	g.Expect(tenant.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-a"))

	var tenantRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aitenant), Namespace: "team-a-maas"}, &tenantRole)).To(Succeed())
	g.Expect(tenantRole.Rules).NotTo(BeEmpty())
	for _, rule := range tenantRole.Rules {
		g.Expect(rule.Verbs).NotTo(ContainElement("*"))
		g.Expect(rule.Resources).NotTo(ContainElement("*"))
		g.Expect(rule.Verbs).NotTo(ContainElement("escalate"))
		g.Expect(rule.Verbs).NotTo(ContainElement("bind"))
		g.Expect(rule.Verbs).NotTo(ContainElement("impersonate"))
	}

	var tenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aitenant), Namespace: "team-a-maas"}, &tenantBinding)).To(Succeed())
	g.Expect(tenantBinding.Subjects).To(ContainElement(rbacv1.Subject{
		Kind:     rbacv1.GroupKind,
		APIGroup: rbacv1.GroupName,
		Name:     "team-a-admins",
	}))

	var aitenantRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: "ai-tenants"}, &aitenantRole)).To(Succeed())
	g.Expect(aitenantRole.Rules).NotTo(BeEmpty())
	for _, rule := range aitenantRole.Rules {
		g.Expect(rule.Verbs).NotTo(ContainElement("*"))
		g.Expect(rule.Resources).NotTo(ContainElement("*"))
		g.Expect(rule.Verbs).NotTo(ContainElement("escalate"))
		g.Expect(rule.Verbs).NotTo(ContainElement("bind"))
		g.Expect(rule.Verbs).NotTo(ContainElement("impersonate"))
	}

	var aitenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: "ai-tenants"}, &aitenantBinding)).To(Succeed())
	g.Expect(aitenantBinding.RoleRef.Name).To(Equal(aitenantAccessRoleName(aitenant)))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("Reconciled"))
}

func TestAITenantGatewaySpec_DefaultsTLSCertificateRefNamespaceToGatewayNamespace(t *testing.T) {
	g := NewWithT(t)
	r := &AITenantReconciler{GatewayNamespace: "openshift-ingress"}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-tls",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-tls-maas"},
			TLS: &maasv1alpha1.AITenantTLSConfig{
				CertificateRef: maasv1alpha1.AITenantTLSCertificateRef{Name: "team-tls-cert"},
			},
		},
	}

	spec := r.gatewaySpecFor(aitenant)

	g.Expect(spec.Listeners).To(HaveLen(2))
	https := spec.Listeners[1]
	g.Expect(https.Name).To(Equal(gatewayapiv1.SectionName("https")))
	g.Expect(https.TLS).NotTo(BeNil())
	g.Expect(https.TLS.CertificateRefs).To(HaveLen(1))
	g.Expect(https.TLS.CertificateRefs[0].Namespace).NotTo(BeNil())
	g.Expect(*https.TLS.CertificateRefs[0].Namespace).To(Equal(gatewayapiv1.Namespace("openshift-ingress")))
}

func TestAITenantErrorHelpersRecognizeAPIStatusReasons(t *testing.T) {
	g := NewWithT(t)

	g.Expect(isNotFoundError(apierrors.NewNotFound(schema.GroupResource{Group: gatewayapiv1.GroupName, Resource: "gateways"}, "team-a"))).To(BeTrue())
	g.Expect(isNotFoundError(apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "team-a"))).To(BeTrue())
	g.Expect(isAlreadyExistsError(apierrors.NewAlreadyExists(schema.GroupResource{Group: gatewayapiv1.GroupName, Resource: "gateways"}, "team-a"))).To(BeTrue())
	g.Expect(isAlreadyExistsError(apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "team-a"))).To(BeTrue())
}

func TestAITenantReconcile_PreExistingNamespaceWithCreateFalse(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	create := false
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-b",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{
				Name:   "team-b-maas",
				Create: &create,
			},
			Gateway: &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b-maas"}}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, existingAITenantGateway("team-b")).
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

	var updatedNS corev1.Namespace
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-b-maas"}, &updatedNS)).To(Succeed())
	g.Expect(updatedNS.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-b"))
	g.Expect(updatedNS.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))
}

func TestAITenantReconcile_CreateFalseMissingNamespaceSetsPending(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	create := false
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-c",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{
				Name:   "team-c-maas",
				Create: &create,
			},
			Gateway: &maasv1alpha1.AITenantGatewayRef{},
		},
	}
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
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Pending"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("TenantNamespaceMissing"))

	var gateway gatewayapiv1.Gateway
	err = cl.Get(context.Background(), client.ObjectKey{Name: "team-c", Namespace: "openshift-ingress"}, &gateway)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAITenantReconcile_RejectsProtectedNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-d",
			Namespace: "opendatahub",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-d-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
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
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
}

func TestAITenantReconcile_RejectsAITenantInDefaultTenantNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-tenant-ns",
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-tenant-ns-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
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
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
	g.Expect(ready.Message).To(ContainSubstring(`not the tenant namespace "models-as-a-service"`))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: "team-tenant-ns-maas"}, &corev1.Namespace{}))).To(BeTrue())
}

func TestAITenantReconcile_RejectsWrongInfraNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-wrong-infra",
			Namespace: "other-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-wrong-infra-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
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
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
	g.Expect(ready.Message).To(ContainSubstring(`configured AITenant infrastructure namespace "ai-tenants"`))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: "team-wrong-infra-maas"}, &corev1.Namespace{}))).To(BeTrue())
}

func TestAITenantReconcile_AllowsDefaultTenantNamespaceFromInfraNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "models-as-a-service",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "models-as-a-service"},
			Gateway: &maasv1alpha1.AITenantGatewayRef{
				Name: "maas-default-gateway",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("maas-default-gateway")).
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
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.TenantNamespace).To(Equal("models-as-a-service"))
	g.Expect(updated.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "maas-default-gateway",
	}))

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "models-as-a-service"}, &tenant)).To(Succeed())
	g.Expect(tenant.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "models-as-a-service"))

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "maas-default-gateway", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "models-as-a-service"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "models-as-a-service"))
}

func TestAITenantReconcile_DeletionCleansUpChildren(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-del",
			Namespace:  "ai-tenants",
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{
				Name: "team-del-maas",
			},
			Gateway: &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
				aitenantCreatedAnnotation:   "true",
			},
			Labels: map[string]string{aitenantManagedLabel: "true"},
		},
	}
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: tenantAdminRoleName(aitenant)},
	}
	objRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: "ai-tenants",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
	}
	objBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: "ai-tenants",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: aitenantAccessRoleName(aitenant)},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, createdAITenantGateway("team-del", "team-del", "ai-tenants"), tenant, role, binding, objRole, objBinding).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	g.Expect(cl.Delete(ctx, aitenant)).To(Succeed())

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "openshift-ingress", Name: "team-del"}, &gatewayapiv1.Gateway{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: maasv1alpha1.TenantInstanceName}, &maasv1alpha1.Tenant{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-tenants", Name: aitenantAccessRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-tenants", Name: aitenantAccessRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	var surviving corev1.Namespace
	g.Expect(cl.Get(ctx, client.ObjectKey{Name: "team-del-maas"}, &surviving)).To(Succeed())
	g.Expect(surviving.Labels).NotTo(HaveKey(aitenantManagedLabel))
	g.Expect(surviving.Labels).NotTo(HaveKey(aiGatewayTenantLabel))
	g.Expect(surviving.Labels).NotTo(HaveKey("opendatahub.io/generated-namespace"))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantNameAnnotation))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantNamespaceAnnotation))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, key, &maasv1alpha1.AITenant{}))).To(BeTrue())
}

func TestAITenantReconcile_DeletionLeavesNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-noclean",
			Namespace:  "ai-tenants",
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-noclean-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-noclean-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-noclean",
				aitenantNamespaceAnnotation: "ai-tenants",
				aitenantCreatedAnnotation:   "true",
			},
			Labels: map[string]string{
				aitenantManagedLabel: "true",
				aiGatewayTenantLabel: "team-noclean",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	g.Expect(cl.Delete(ctx, aitenant)).To(Succeed())

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())

	var surviving corev1.Namespace
	g.Expect(cl.Get(ctx, client.ObjectKey{Name: "team-noclean-maas"}, &surviving)).To(Succeed())
	g.Expect(surviving.Labels).NotTo(HaveKey(aitenantManagedLabel))
	g.Expect(surviving.Labels).NotTo(HaveKey(aiGatewayTenantLabel))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantNameAnnotation))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantNamespaceAnnotation))
	g.Expect(surviving.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))
}

func TestAITenantReconcile_DeletionPreservesAdoptedGateway(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	now := metav1.Now()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "team-adopt-del",
			Namespace:         "ai-tenants",
			Finalizers:        []string{aitenantFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-adopt-del-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	gateway := ownedAITenantGateway("team-adopt-del", "team-adopt-del", "ai-tenants")
	applyAITenantMetadata(gateway, aitenant)

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

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var gatewayAfter gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: "openshift-ingress", Name: "team-adopt-del"}, &gatewayAfter)).To(Succeed())
	g.Expect(gatewayAfter.Labels).NotTo(HaveKey(aitenantManagedLabel))
	g.Expect(gatewayAfter.Labels).NotTo(HaveKey(aiGatewayTenantLabel))
	g.Expect(gatewayAfter.Annotations).NotTo(HaveKey(aitenantNameAnnotation))
	g.Expect(gatewayAfter.Annotations).NotTo(HaveKey(aitenantNamespaceAnnotation))
	g.Expect(gatewayAfter.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))
}

func TestAITenantReconcile_AdoptsPreExistingGateway(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adopt",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-adopt-maas"},
			Gateway: &maasv1alpha1.AITenantGatewayRef{
				Name: "existing-gw",
			},
		},
	}
	preExistingGW := &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-gw",
			Namespace: "openshift-ingress",
		},
		Spec: gatewayapiv1.GatewaySpec{
			GatewayClassName: "openshift-default",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, preExistingGW).
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

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "existing-gw", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-adopt"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-adopt"))
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aitenantNamespaceAnnotation, "ai-tenants"))
	g.Expect(gateway.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))
	g.Expect(gateway.Spec.Listeners).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[0].Name).To(Equal(gatewayapiv1.SectionName("http")))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.GatewayRef.Name).To(Equal("existing-gw"))
}

func TestAITenantUpsert_PatchesAfterCreateAlreadyExistsRace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-race",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-race-maas"},
		},
	}
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-child",
			Namespace: "team-race-maas",
		},
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-child",
			Namespace: "team-race-maas",
		},
	}
	simulateCreateRace := false
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && simulateCreateRace {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	g.Expect(cl.Create(context.Background(), existing)).To(Succeed())
	simulateCreateRace = true
	r := &AITenantReconciler{
		Client:    cl,
		Scheme:    s,
		APIReader: &firstNotFoundReader{Reader: cl, first: true, resource: schema.GroupResource{Resource: "configmaps"}},
	}

	err := r.upsert(context.Background(), desired, aitenant, func(obj client.Object) error {
		configMap, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return fmt.Errorf("expected ConfigMap, got %T", obj)
		}
		applyAITenantMetadata(configMap, aitenant)
		configMap.Data = map[string]string{"patched": "true"}
		return nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated corev1.ConfigMap
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: "team-race-maas", Name: "race-child"}, &updated)).To(Succeed())
	g.Expect(updated.Data).To(HaveKeyWithValue("patched", "true"))
	g.Expect(updated.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-race"))
	g.Expect(updated.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-race"))
}

func TestAITenantReconcile_AdoptsPreExistingTenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adoptcfg",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-adoptcfg-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/adoptcfg",
				ClientID:  "adoptcfg-client",
			},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-adoptcfg-maas"}}
	preExistingTenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "team-adoptcfg-maas",
		},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "old-ns",
				Name:      "old-gw",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, preExistingTenant, existingAITenantGateway("team-adoptcfg")).
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

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-adoptcfg-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-adoptcfg"))
	g.Expect(tenant.Spec.GatewayRef.Namespace).To(Equal("openshift-ingress"))
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://issuer.example.com/realms/adoptcfg"))
}

func TestAITenantReconcile_RejectsNamespaceOwnedByAnotherAITenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-clash",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "shared-ns"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-ns",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "other-aitenant",
				aitenantNamespaceAnnotation: "ai-tenants",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns).
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
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("TenantNamespaceFailed"))
	g.Expect(ready.Message).To(ContainSubstring("another AITenant"))
}

func TestAITenantReconcile_RejectsGatewayOwnedByAnotherAITenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-gw-clash",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-gw-clash-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	gateway := ownedAITenantGateway("team-gw-clash", "other-aitenant", "ai-tenants")
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

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("GatewayCreationFailed"))
	g.Expect(ready.Message).To(ContainSubstring("another AITenant"))
}

func TestAITenantReconcile_CreatesCustomNamedGatewayWhenMissing(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-missing-gw",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-missing-gw-maas"},
			Gateway: &maasv1alpha1.AITenantGatewayRef{
				Name: "missing-gw",
			},
		},
	}
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
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "missing-gw",
	}))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("Reconciled"))

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "missing-gw", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-missing-gw"))
}

func TestAITenantReconcile_IdempotentOnSecondRun(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-idem",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-idem-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-idem")).
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

	var afterFirst maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &afterFirst)).To(Succeed())
	g.Expect(afterFirst.Status.Phase).To(Equal("Active"))

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var afterSecond maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &afterSecond)).To(Succeed())
	g.Expect(afterSecond.Status.Phase).To(Equal("Active"))
	g.Expect(afterSecond.ResourceVersion).To(Equal(afterFirst.ResourceVersion))
}

func TestAITenantReconcile_OIDCFullMirror(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-oidc",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-oidc-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://sso.corp.example.com/realms/ai",
				ClientID:  "ai-platform",
				TTL:       600,
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-oidc")).
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

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-oidc-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://sso.corp.example.com/realms/ai"))
	g.Expect(tenant.Spec.ExternalOIDC.ClientID).To(Equal("ai-platform"))
	g.Expect(tenant.Spec.ExternalOIDC.TTL).To(Equal(600))
}

func TestAITenantReconcile_NoOIDCSetsTenantOIDCNil(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-nooidc",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-nooidc-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-nooidc")).
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

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-nooidc-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.ExternalOIDC).To(BeNil())
}

func TestAITenantChildName_Truncation(t *testing.T) {
	g := NewWithT(t)

	short := aitenantChildName("team-a", "tenant-admin")
	g.Expect(short).To(Equal("aitenant-team-a-tenant-admin"))
	g.Expect(len(short)).To(BeNumerically("<=", 63))

	longName := "this-is-a-very-long-aitenant-name-that-exceeds-the-k8s-limit"
	truncated := aitenantChildName(longName, "tenant-admin")
	g.Expect(len(truncated)).To(BeNumerically("<=", 63))
	g.Expect(truncated).To(HavePrefix("aitenant-"))
	g.Expect(truncated).To(ContainSubstring("tenant-admin"))

	g.Expect(aitenantChildName(longName, "tenant-admin")).To(Equal(truncated))
}

func TestAITenantReconcile_RejectsTenantNamespaceEqualToAITenantNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-samens",
			Namespace: "ai-tenants",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "ai-tenants"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{},
		},
	}
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
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
}
