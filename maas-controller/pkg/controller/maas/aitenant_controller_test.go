//nolint:testpackage
package maas

import (
	"context"
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
				"platform.opendatahub.io/owner": "network-admin",
			},
			Annotations: map[string]string{
				"network.opendatahub.io/ticket": "approved",
			},
		},
		Spec: gatewayapiv1.GatewaySpec{
			GatewayClassName: gatewayapiv1.ObjectName("openshift-default"),
		},
	}
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
	g := NewWithT(t)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestAITenantReconcile_ValidatesExistingGatewayAndCreatesBootstrapResources(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-a-maas"},
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

	var updatedGateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-a", Namespace: "openshift-ingress"}, &updatedGateway)).To(Succeed())
	g.Expect(updatedGateway.Labels).To(HaveKeyWithValue("platform.opendatahub.io/owner", "network-admin"))
	g.Expect(updatedGateway.Labels).NotTo(HaveKey(aiGatewayTenantLabel))
	g.Expect(updatedGateway.Labels).NotTo(HaveKey(aitenantManagedLabel))
	g.Expect(updatedGateway.Annotations).To(HaveKeyWithValue("network.opendatahub.io/ticket", "approved"))
	g.Expect(updatedGateway.Annotations).NotTo(HaveKey(aitenantNameAnnotation))
	g.Expect(updatedGateway.Annotations).NotTo(HaveKey(aitenantNamespaceAnnotation))
	g.Expect(updatedGateway.Spec).To(Equal(gateway.Spec))

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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: "redhat-ai-gateway-infra"}, &aitenantRole)).To(Succeed())
	g.Expect(aitenantRole.Rules).NotTo(BeEmpty())

	var aitenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: "redhat-ai-gateway-infra"}, &aitenantBinding)).To(Succeed())
	g.Expect(aitenantBinding.RoleRef.Name).To(Equal(aitenantAccessRoleName(aitenant)))

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "team-a",
	}))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("Reconciled"))
}

func TestAITenantReconcile_MissingGatewaySetsFailedStatus(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-missing-gw",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-missing-gw-maas"},
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
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	g.Expect(updated.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "team-missing-gw",
	}))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("GatewayCheckFailed"))
	g.Expect(ready.Message).To(ContainSubstring("must be created by a network or cluster administrator"))

	var tenant maasv1alpha1.Tenant
	err = cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-missing-gw-maas"}, &tenant)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	var ns corev1.Namespace
	err = cl.Get(context.Background(), client.ObjectKey{Name: "team-missing-gw-maas"}, &ns)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAITenantReconcile_ExplicitGatewayNameResolvesExistingGateway(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-explicit",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-explicit-maas"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{Name: "network-approved-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("network-approved-gw")).
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
	g.Expect(updated.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "network-approved-gw",
	}))

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-explicit-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.GatewayRef.Name).To(Equal("network-approved-gw"))
}

func TestAITenantReconcile_UpdatesPreExistingTenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adoptcfg",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-adoptcfg-maas"},
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
				Namespace: "old-gateway-ns",
				Name:      "old-gateway",
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
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "team-adoptcfg",
	}))
	g.Expect(tenant.Spec.ExternalOIDC).To(Equal(aitenant.Spec.OIDC))
}

func TestAITenantReconcile_PreExistingNamespaceWithCreateFalse(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	create := false
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-b",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{
				Name:   "team-b-maas",
				Create: &create,
			},
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
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{
				Name:   "team-c-maas",
				Create: &create,
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-c")).
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
	g.Expect(ready.Message).To(ContainSubstring(`configured AITenant infrastructure namespace "redhat-ai-gateway-infra"`))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: "team-wrong-infra-maas"}, &corev1.Namespace{}))).To(BeTrue())
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

func TestAITenantReconcile_RejectsTenantNamespaceEqualToAITenantNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-samens",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "redhat-ai-gateway-infra"},
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
	g.Expect(ready.Message).To(ContainSubstring("must be different from the AITenant infra namespace"))
}

func TestAITenantReconcile_AllowsDefaultTenantNamespaceFromInfraNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "models-as-a-service",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "models-as-a-service"},
			Gateway:         &maasv1alpha1.AITenantGatewayRef{Name: "maas-default-gateway"},
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
}

func TestAITenantReconcile_IdempotentWhenActive(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-idem",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-idem-maas"},
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

	var afterActive maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &afterActive)).To(Succeed())
	g.Expect(afterActive.Status.Phase).To(Equal("Active"))

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var afterRepeat maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &afterRepeat)).To(Succeed())
	g.Expect(afterRepeat.Status.Phase).To(Equal("Active"))
	g.Expect(afterRepeat.Status).To(Equal(afterActive.Status))
}

func TestAITenantReconcile_RejectsNamespaceOwnedByAnotherAITenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-conflict",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "shared-ns"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-ns",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "other-aitenant",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, existingAITenantGateway("team-conflict")).
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

func TestAITenantReconcile_DeletionCleansChildrenButLeavesGatewayUntouched(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-del",
			Namespace:  "redhat-ai-gateway-infra",
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-del-maas"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
				aitenantCreatedAnnotation:   "true",
			},
			Labels: map[string]string{
				aitenantManagedLabel:                   "true",
				aiGatewayTenantLabel:                   "team-del",
				"opendatahub.io/generated-namespace":   "true",
				"maas.opendatahub.io/tenant-name":      "team-del",
				"maas.opendatahub.io/tenant-namespace": "team-del-maas",
			},
		},
	}
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: tenantAdminRoleName(aitenant)},
	}
	objRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: "redhat-ai-gateway-infra",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
	}
	objBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: "redhat-ai-gateway-infra",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: "redhat-ai-gateway-infra",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: aitenantAccessRoleName(aitenant)},
	}
	gateway := existingAITenantGateway("team-del")
	gateway.Labels[aiGatewayTenantLabel] = "preexisting-value"
	gateway.Annotations[aitenantNameAnnotation] = "team-del"

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, gateway, tenant, role, binding, objRole, objBinding).
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

	var survivingGateway gatewayapiv1.Gateway
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: "openshift-ingress", Name: "team-del"}, &survivingGateway)).To(Succeed())
	g.Expect(survivingGateway.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "preexisting-value"))
	g.Expect(survivingGateway.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-del"))

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: maasv1alpha1.TenantInstanceName}, &maasv1alpha1.Tenant{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "redhat-ai-gateway-infra", Name: aitenantAccessRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "redhat-ai-gateway-infra", Name: aitenantAccessRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())

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

func TestAITenantReconcile_RBACServiceAccountRequiresNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-sa",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-sa-maas"},
			RBAC: &maasv1alpha1.AITenantRBACConfig{
				Admins: []maasv1alpha1.AITenantRBACSubject{{
					Kind: rbacv1.ServiceAccountKind,
					Name: "tenant-admin",
				}},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-sa")).
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
	g.Expect(ready.Reason).To(Equal("RBACReconcileFailed"))
	g.Expect(ready.Message).To(ContainSubstring("namespace is required for ServiceAccount"))
}

func TestAITenantUpsert_PatchesAfterCreateAlreadyExistsRace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-race",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-race-maas"},
		},
	}
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-child",
			Namespace: "team-race-maas",
			Labels: map[string]string{
				"stale": "true",
			},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(existing).
		Build()
	reader := &firstNotFoundReader{
		Reader:   baseClient,
		first:    true,
		resource: schema.GroupResource{Resource: "configmaps"},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
			},
		}).
		Build()
	r := &AITenantReconciler{
		Client:    cl,
		Scheme:    s,
		APIReader: reader,
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-child",
			Namespace: "team-race-maas",
		},
	}
	err := r.upsert(context.Background(), configMap, aitenant, func(obj client.Object) error {
		applyAITenantMetadata(obj, aitenant)
		cm, ok := obj.(*corev1.ConfigMap)
		g.Expect(ok).To(BeTrue())
		cm.Data = map[string]string{"fresh": "true"}
		return nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated corev1.ConfigMap
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: "team-race-maas", Name: "race-child"}, &updated)).To(Succeed())
	g.Expect(updated.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-race"))
	g.Expect(updated.Data).To(HaveKeyWithValue("fresh", "true"))
}

func TestAITenantReconcile_OIDCFullMirror(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-oidc",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-oidc-maas"},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/team-oidc",
				ClientID:  "team-oidc-client",
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
	g.Expect(tenant.Spec.ExternalOIDC).To(Equal(aitenant.Spec.OIDC))
}

func TestAITenantReconcile_NoOIDCSetsTenantOIDCNil(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-nooidc",
			Namespace: "redhat-ai-gateway-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{
			TenantNamespace: maasv1alpha1.AITenantTenantNamespace{Name: "team-nooidc-maas"},
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
	name := "tenant-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz-abcdefghijklmnopqrstuvwxyz"

	got := aitenantChildName(name, aitenantTenantAdminRoleSuffix)
	g.Expect(len(got)).To(BeNumerically("<=", 63))
	g.Expect(got).To(HavePrefix("aitenant-tenant-"))
	g.Expect(got).To(ContainSubstring("-tenant-admin-"))
}
