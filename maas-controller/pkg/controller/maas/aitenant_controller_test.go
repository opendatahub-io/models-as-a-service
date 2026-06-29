//nolint:testpackage
package maas

import (
	"context"
	"strings"
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
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

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
	g.Expect(res.RequeueAfter).To(Equal(time.Second))

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
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "ai-tenant-team-a"}, &ns)).To(Succeed())
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aitenantCreatedAnnotation, "true"))
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("opendatahub.io/generated-namespace", "true"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue(aitenantManagedLabel, "true"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-name", "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-namespace", "ai-tenant-team-a"))

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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-a"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{}))
	g.Expect(tenant.Spec.ExternalOIDC).To(BeNil())
	g.Expect(tenant.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-a"))
	g.Expect(tenant.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-a"))
	g.Expect(tenant.Annotations).To(HaveKeyWithValue(aitenantNamespaceAnnotation, tenantreconcile.DefaultAITenantNamespace))

	var tenantRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aitenant), Namespace: "ai-tenant-team-a"}, &tenantRole)).To(Succeed())
	g.Expect(tenantRole.Rules).NotTo(BeEmpty())
	for _, rule := range tenantRole.Rules {
		g.Expect(rule.Verbs).NotTo(ContainElement("*"))
		g.Expect(rule.Resources).NotTo(ContainElement("*"))
		g.Expect(rule.Verbs).NotTo(ContainElement("escalate"))
		g.Expect(rule.Verbs).NotTo(ContainElement("bind"))
		g.Expect(rule.Verbs).NotTo(ContainElement("impersonate"))
	}

	var tenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aitenant), Namespace: "ai-tenant-team-a"}, &tenantBinding)).To(Succeed())
	g.Expect(tenantBinding.Subjects).To(ContainElement(rbacv1.Subject{
		Kind:     rbacv1.GroupKind,
		APIGroup: rbacv1.GroupName,
		Name:     "team-a-admins",
	}))

	var aitenantRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: tenantreconcile.DefaultAITenantNamespace}, &aitenantRole)).To(Succeed())
	g.Expect(aitenantRole.Rules).NotTo(BeEmpty())

	var aitenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aitenantAccessRoleName(aitenant), Namespace: tenantreconcile.DefaultAITenantNamespace}, &aitenantBinding)).To(Succeed())
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

func TestAITenantReconcile_PersistsGatewayStatusBeforeTenantCreate(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
	}
	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	gateway := existingAITenantGateway("team-a")

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, gateway).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*maasv1alpha1.Tenant); ok {
					var current maasv1alpha1.AITenant
					g.Expect(c.Get(ctx, key, &current)).To(Succeed())
					g.Expect(current.Status.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
						Name:      "team-a",
						Namespace: "openshift-ingress",
					}))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	reconcileAITenantTwice(t, r, key)
}

func TestAITenantReconcile_MissingGatewaySetsFailedStatus(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-missing-gw",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
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
	g.Expect(res.RequeueAfter).To(Equal(time.Second))

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
	err = cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-missing-gw"}, &tenant)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	var ns corev1.Namespace
	err = cl.Get(context.Background(), client.ObjectKey{Name: "ai-tenant-team-missing-gw"}, &ns)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAITenantReconcile_ExplicitGatewayNameResolvesExistingGateway(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-explicit",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "network-approved-gw"},
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-explicit"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{}))
}

func TestAITenantReconcile_UpdatesPreExistingTenant(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adoptcfg",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/adoptcfg",
				ClientID:  "adoptcfg-client",
			},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ai-tenant-team-adoptcfg"}}
	preExistingTenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-team-adoptcfg",
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-adoptcfg"}, &tenant)).To(Succeed())
	g.Expect(tenant.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-adoptcfg"))
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "old-gateway-ns",
		Name:      "old-gateway",
	}))
	g.Expect(tenant.Spec.ExternalOIDC).To(BeNil())
}

func TestAITenantReconcile_LabelsPreExistingDerivedNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-b",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ai-tenant-team-b"}}
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "ai-tenant-team-b"}, &updatedNS)).To(Succeed())
	g.Expect(updatedNS.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-b"))
	g.Expect(updatedNS.Annotations).NotTo(HaveKey(aitenantCreatedAnnotation))
}

func TestAITenantReconcile_RejectsWrongInfraNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-wrong-infra",
			Namespace: "other-infra",
		},
		Spec: maasv1alpha1.AITenantSpec{},
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
	g.Expect(ready.Message).To(ContainSubstring(`configured AITenant infrastructure namespace "` + tenantreconcile.DefaultAITenantNamespace + `"`))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: "ai-tenant-team-wrong-infra"}, &corev1.Namespace{}))).To(BeTrue())
}

func TestAITenantReconcile_RejectsProtectedNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-d",
			Namespace: "opendatahub",
		},
		Spec: maasv1alpha1.AITenantSpec{},
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

func TestAITenantReconcile_RejectsDerivedNamespaceOverDNSLabelLimit(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenantName := strings.Repeat("a", 54)
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
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
	g.Expect(ready.Message).To(ContainSubstring("derived tenant namespace"))
	g.Expect(ready.Message).To(ContainSubstring("must be no more than 63 characters"))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: tenantreconcile.TenantNamespaceForAITenant(aitenantName, "models-as-a-service")}, &corev1.Namespace{}))).To(BeTrue())
}

func TestAITenantReconcile_AllowsDefaultTenantNamespaceFromInfraNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "models-as-a-service",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "maas-default-gateway"},
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

func TestAITenantReconcile_DefaultAITenantUsesConfiguredTenantNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "models-as-a-service",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "maas-default-gateway"},
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
		TenantNamespace:  "custom-maas",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	reconcileAITenantTwice(t, r, key)

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.TenantNamespace).To(Equal("custom-maas"))

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "custom-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "models-as-a-service"))
	g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "models-as-a-service"}, &maasv1alpha1.Tenant{}))).To(BeTrue())
}

func TestAITenantReconcile_IdempotentWhenActive(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-idem",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
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
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ai-tenant-team-conflict",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "other-aitenant",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
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
	g.Expect(res.RequeueAfter).To(Equal(time.Second))

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
			Namespace:  tenantreconcile.DefaultAITenantNamespace,
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ai-tenant-team-del",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
				aitenantCreatedAnnotation:   "true",
			},
			Labels: map[string]string{
				aitenantManagedLabel:                   "true",
				aiGatewayTenantLabel:                   "team-del",
				"opendatahub.io/generated-namespace":   "true",
				"maas.opendatahub.io/tenant-name":      "team-del",
				"maas.opendatahub.io/tenant-namespace": "ai-tenant-team-del",
			},
		},
	}
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-team-del",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "ai-tenant-team-del",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: "ai-tenant-team-del",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: tenantAdminRoleName(aitenant)},
	}
	objRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	objBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-del",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
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

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-tenant-team-del", Name: maasv1alpha1.TenantInstanceName}, &maasv1alpha1.Tenant{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-tenant-team-del", Name: tenantAdminRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-tenant-team-del", Name: tenantAdminRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: aitenantAccessRoleName(aitenant)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: aitenantAccessRoleName(aitenant)}, &rbacv1.RoleBinding{}))).To(BeTrue())

	var surviving corev1.Namespace
	g.Expect(cl.Get(ctx, client.ObjectKey{Name: "ai-tenant-team-del"}, &surviving)).To(Succeed())
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
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
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
	g.Expect(res.RequeueAfter).To(Equal(time.Second))

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
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
	}
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "race-child",
			Namespace: "ai-tenant-team-race",
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
			Namespace: "ai-tenant-team-race",
		},
	}
	err := r.upsert(context.Background(), configMap, aitenant, func(obj client.Object) error {
		applyAITenantMetadata(obj, aitenant, tenantreconcile.TenantNamespaceForAITenant(aitenant.Name, ""))
		cm, ok := obj.(*corev1.ConfigMap)
		g.Expect(ok).To(BeTrue())
		cm.Data = map[string]string{"fresh": "true"}
		return nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated corev1.ConfigMap
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: "ai-tenant-team-race", Name: "race-child"}, &updated)).To(Succeed())
	g.Expect(updated.Labels).To(HaveKeyWithValue(aiGatewayTenantLabel, "team-race"))
	g.Expect(updated.Data).To(HaveKeyWithValue("fresh", "true"))
}

func TestAITenantReconcile_OIDCStaysInAITenantSpec(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-oidc",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-oidc"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.ExternalOIDC).To(BeNil())
}

func TestAITenantReconcile_NoOIDCSetsTenantOIDCNil(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-nooidc",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
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
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "ai-tenant-team-nooidc"}, &tenant)).To(Succeed())
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

func TestGatewayClaimName_Deterministic(t *testing.T) {
	g := NewWithT(t)

	ref := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "team-a"}
	name1 := gatewayClaimName(ref)
	name2 := gatewayClaimName(ref)
	g.Expect(name1).To(Equal(name2))
	g.Expect(name1).To(HavePrefix("gateway-claim-"))
	g.Expect(len(name1)).To(BeNumerically("<=", 63))
}

func TestGatewayClaimName_UniquenessAcrossRefs(t *testing.T) {
	g := NewWithT(t)

	refA := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "gw-a"}
	refB := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "gw-b"}
	g.Expect(gatewayClaimName(refA)).NotTo(Equal(gatewayClaimName(refB)))
}

func TestAITenantReconcile_GatewayClaimCreated(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-claim",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "team-claim-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("team-claim-gw")).
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

	// Verify the gateway claim ConfigMap was created.
	gatewayRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "team-claim-gw"}
	claimName := gatewayClaimName(gatewayRef)
	var claim corev1.ConfigMap
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &claim)).To(Succeed())
	g.Expect(claim.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-claim"))
	g.Expect(claim.Data).To(HaveKeyWithValue("gatewayNamespace", "openshift-ingress"))
	g.Expect(claim.Data).To(HaveKeyWithValue("gatewayName", "team-claim-gw"))
}

func TestAITenantReconcile_GatewayClaimBlocksDuplicateGateway(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	// Create the first AITenant and reconcile it to establish its claim.
	aitenant1 := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-first",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "shared-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant1, existingAITenantGateway("shared-gw")).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key1 := types.NamespacedName{Name: aitenant1.Name, Namespace: aitenant1.Namespace}
	reconcileAITenantTwice(t, r, key1)

	var updated1 maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key1, &updated1)).To(Succeed())
	g.Expect(updated1.Status.Phase).To(Equal("Active"))

	// Now create a second AITenant referencing the same gateway.
	aitenant2 := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-second",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "shared-gw"},
		},
	}
	g.Expect(cl.Create(context.Background(), aitenant2)).To(Succeed())

	key2 := types.NamespacedName{Name: aitenant2.Name, Namespace: aitenant2.Namespace}

	// First reconcile adds the finalizer.
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key2})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(time.Second))

	// Second reconcile should fail due to gateway claim conflict.
	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key2})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated2 maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key2, &updated2)).To(Succeed())
	g.Expect(updated2.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated2.Status.Conditions, maasv1alpha1.AITenantConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("GatewayClaimFailed"))
	g.Expect(ready.Message).To(ContainSubstring("already claimed"))
	g.Expect(ready.Message).To(ContainSubstring("team-first"))
}

func TestAITenantReconcile_GatewayClaimCleanedOnDeletion(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-cleanup",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "cleanup-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("cleanup-gw")).
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

	// Verify claim exists.
	gatewayRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "cleanup-gw"}
	claimName := gatewayClaimName(gatewayRef)
	var claim corev1.ConfigMap
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &claim)).To(Succeed())

	// Delete the AITenant and reconcile.
	var toDelete maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &toDelete)).To(Succeed())
	g.Expect(cl.Delete(ctx, &toDelete)).To(Succeed())

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Verify claim is deleted.
	err = cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &corev1.ConfigMap{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAITenantReconcile_GatewayClaimIdempotent(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-idem-claim",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "idem-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("idem-gw")).
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
	g.Expect(cl.Get(context.Background(), key, &afterFirst)).To(Succeed())
	g.Expect(afterFirst.Status.Phase).To(Equal("Active"))

	// Third reconcile should be idempotent -- claim already exists and owned by this AITenant.
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var afterRepeat maasv1alpha1.AITenant
	g.Expect(cl.Get(context.Background(), key, &afterRepeat)).To(Succeed())
	g.Expect(afterRepeat.Status.Phase).To(Equal("Active"))
}

func TestAITenantReconcile_GatewayClaimHasOwnerReference(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-ownerref",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "ownerref-gw"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("ownerref-gw")).
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

	// Verify the gateway claim ConfigMap has an OwnerReference pointing to the AITenant.
	gatewayRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "ownerref-gw"}
	claimName := gatewayClaimName(gatewayRef)
	var claim corev1.ConfigMap
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &claim)).To(Succeed())
	g.Expect(claim.OwnerReferences).To(HaveLen(1))
	g.Expect(claim.OwnerReferences[0].Name).To(Equal("team-ownerref"))
	g.Expect(claim.OwnerReferences[0].Kind).To(Equal("AITenant"))
	isController := true
	g.Expect(claim.OwnerReferences[0].Controller).To(Equal(&isController))
}

func TestAITenantReconcile_GatewayClaimRetroactiveOwnerReference(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-retroactive",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       "retro-uid-1234",
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "retro-gw"},
		},
	}
	gatewayRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "retro-gw"}
	claimName := gatewayClaimName(gatewayRef)

	// Pre-create a claim ConfigMap WITHOUT an OwnerReference, simulating a
	// claim created before the OwnerReference feature was deployed.
	preExistingClaim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":      "maas-controller",
				"maas.opendatahub.io/gateway-claim": "true",
				aitenantManagedLabel:                "true",
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-retroactive",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			// No OwnerReferences set.
		},
		Data: map[string]string{
			"gatewayNamespace": "openshift-ingress",
			"gatewayName":      "retro-gw",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("retro-gw"), preExistingClaim).
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

	// Verify the pre-existing claim has no OwnerReferences.
	var before corev1.ConfigMap
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &before)).To(Succeed())
	g.Expect(before.OwnerReferences).To(BeEmpty())

	// Reconcile the AITenant -- the controller should retroactively add the
	// OwnerReference to the existing claim.
	reconcileAITenantTwice(t, r, key)

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))

	// Verify the OwnerReference was retroactively added.
	var after corev1.ConfigMap
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: claimName}, &after)).To(Succeed())
	g.Expect(after.OwnerReferences).To(HaveLen(1))
	g.Expect(after.OwnerReferences[0].Name).To(Equal("team-retroactive"))
	g.Expect(after.OwnerReferences[0].Kind).To(Equal("AITenant"))
	isController := true
	g.Expect(after.OwnerReferences[0].Controller).To(Equal(&isController))
}

func TestAITenantReconcile_StaleClaimCleanedOnGatewayRetarget(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	// Create an AITenant pointing to gateway-old and reconcile it.
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-retarget",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "gateway-old"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, existingAITenantGateway("gateway-old"), existingAITenantGateway("gateway-new")).
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

	// Verify old claim exists.
	oldRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "gateway-old"}
	oldClaimName := gatewayClaimName(oldRef)
	var oldClaim corev1.ConfigMap
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: oldClaimName}, &oldClaim)).To(Succeed())

	// Retarget the AITenant to gateway-new.
	var current maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, key, &current)).To(Succeed())
	base := current.DeepCopy()
	current.Spec.Gateway = &maasv1alpha1.AITenantGatewayRef{Name: "gateway-new"}
	g.Expect(cl.Patch(ctx, &current, client.MergeFrom(base))).To(Succeed())

	// Reconcile again -- this should create the new claim and clean up the old one.
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Verify new claim exists.
	newRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "gateway-new"}
	newClaimName := gatewayClaimName(newRef)
	var newClaim corev1.ConfigMap
	g.Expect(cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: newClaimName}, &newClaim)).To(Succeed())
	g.Expect(newClaim.Annotations).To(HaveKeyWithValue(aitenantNameAnnotation, "team-retarget"))

	// Verify old claim was deleted.
	err = cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: oldClaimName}, &corev1.ConfigMap{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAITenantReconcile_DeletionCleansAllClaimsIncludingStale(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	// Simulate a stale claim left from a prior gateway reference plus the current claim.
	staleRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "stale-gw"}
	staleClaim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayClaimName(staleRef),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"maas.opendatahub.io/gateway-claim": "true",
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-delall",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	currentRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "current-gw"}
	currentClaim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayClaimName(currentRef),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"maas.opendatahub.io/gateway-claim": "true",
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-delall",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-delall",
			Namespace:  tenantreconcile.DefaultAITenantNamespace,
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "current-gw"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ai-tenant-team-delall",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-delall",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, staleClaim, currentClaim, existingAITenantGateway("current-gw")).
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
	g.Expect(cl.Delete(ctx, aitenant)).To(Succeed())

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Both claims should be deleted.
	err = cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: gatewayClaimName(staleRef)}, &corev1.ConfigMap{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	err = cl.Get(ctx, client.ObjectKey{Namespace: tenantreconcile.DefaultAITenantNamespace, Name: gatewayClaimName(currentRef)}, &corev1.ConfigMap{})
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestIsClaimOwnedByAITenant_OwnerRefTakesPrecedenceOverAnnotations(t *testing.T) {
	g := NewWithT(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-legit",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       "uid-legit",
		},
	}
	isController := true

	// Case 1: Matching OwnerReference and annotations → owned.
	claimOwned := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-legit",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "AITenant",
				Name:       "team-legit",
				UID:        "uid-legit",
				Controller: &isController,
			}},
		},
	}
	g.Expect(isClaimOwnedByAITenant(claimOwned, aitenant)).To(BeTrue())

	// Case 2: Annotations match but OwnerReference points to a different AITenant
	// (e.g. spoofed annotations or TOCTOU swap) → rejected.
	claimSpoofed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-legit",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "AITenant",
				Name:       "team-other",
				UID:        "uid-other",
				Controller: &isController,
			}},
		},
	}
	g.Expect(isClaimOwnedByAITenant(claimSpoofed, aitenant)).To(BeFalse())

	// Case 3: No OwnerReference (legacy claim) with matching annotations → owned.
	claimLegacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-legit",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	g.Expect(isClaimOwnedByAITenant(claimLegacy, aitenant)).To(BeTrue())

	// Case 4: OwnerReference with matching name but mismatched UID → rejected.
	claimWrongUID := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-legit",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "AITenant",
				Name:       "team-legit",
				UID:        "uid-different",
				Controller: &isController,
			}},
		},
	}
	g.Expect(isClaimOwnedByAITenant(claimWrongUID, aitenant)).To(BeFalse())
}

func TestAITenantReconcile_DeleteGatewayClaimSkipsSpoofedOwnerRef(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-delete-spoof",
			Namespace:  tenantreconcile.DefaultAITenantNamespace,
			UID:        "uid-delete-spoof",
			Finalizers: []string{aitenantFinalizer},
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "del-gw"},
		},
	}

	isController := true
	gwRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "del-gw"}

	// Create a claim with matching annotations but OwnerReference pointing to
	// a different AITenant (spoofed annotations). deleteGatewayClaim must skip it.
	spoofedClaim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayClaimName(gwRef),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"maas.opendatahub.io/gateway-claim": "true",
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-delete-spoof",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "AITenant",
				Name:       "team-real-owner",
				UID:        "uid-real-owner",
				Controller: &isController,
			}},
		},
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ai-tenant-team-delete-spoof",
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-delete-spoof",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, ns, spoofedClaim, existingAITenantGateway("del-gw")).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	// Delete the AITenant and reconcile.
	key := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	g.Expect(cl.Delete(ctx, aitenant)).To(Succeed())

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())

	// The spoofed claim must NOT have been deleted because its OwnerReference
	// points to a different AITenant.
	var remaining corev1.ConfigMap
	err = cl.Get(ctx, client.ObjectKey{
		Namespace: tenantreconcile.DefaultAITenantNamespace,
		Name:      gatewayClaimName(gwRef),
	}, &remaining)
	g.Expect(err).NotTo(HaveOccurred(), "spoofed claim should survive deleteGatewayClaim")
}

func TestEnsureGatewayClaim_RejectsEmptyNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-empty-ns",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       "uid-empty-ns",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(aitenant).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		GatewayNamespace: "openshift-ingress",
	}

	err := r.ensureGatewayClaim(context.Background(), aitenant, maasv1alpha1.TenantGatewayRef{
		Namespace: "",
		Name:      "some-gateway",
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("gateway reference must have both namespace and name set"))
	g.Expect(err.Error()).To(ContainSubstring(`namespace=""`))
}

func TestEnsureGatewayClaim_RejectsEmptyName(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-empty-name",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       "uid-empty-name",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(aitenant).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		GatewayNamespace: "openshift-ingress",
	}

	err := r.ensureGatewayClaim(context.Background(), aitenant, maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "",
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("gateway reference must have both namespace and name set"))
	g.Expect(err.Error()).To(ContainSubstring(`name=""`))
}

func TestGatewayClaimName_DistinguishesNamespaceFromName(t *testing.T) {
	g := NewWithT(t)

	// Verify that "ns-a/gw" and "ns/a-gw" produce different claim names.
	// This ensures the "/" separator prevents cross-field collisions.
	refA := maasv1alpha1.TenantGatewayRef{Namespace: "ns-a", Name: "gw"}
	refB := maasv1alpha1.TenantGatewayRef{Namespace: "ns", Name: "a-gw"}
	g.Expect(gatewayClaimName(refA)).NotTo(Equal(gatewayClaimName(refB)))

	// Also verify that empty namespace would collide without the guard:
	// "/gw-a" and "/gw-a" are trivially equal, but "x/gw-a" and "/xgw-a"
	// would differ because the separator is part of the hash input.
	refC := maasv1alpha1.TenantGatewayRef{Namespace: "x", Name: "gw-a"}
	refD := maasv1alpha1.TenantGatewayRef{Namespace: "", Name: "xgw-a"}
	g.Expect(gatewayClaimName(refC)).NotTo(Equal(gatewayClaimName(refD)),
		"hash includes separator so different field splits produce different names")
}

func TestAITenantReconcile_CleanupStaleClaimsSkipsSpoofedOwnerRef(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-cleanup-spoof",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       "uid-cleanup-spoof",
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: "new-gw"},
		},
	}

	isController := true
	staleRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "old-gw"}

	// Create a stale claim with matching annotations but OwnerReference pointing
	// to a different AITenant. cleanupStaleClaims must skip it.
	spoofedStaleClaim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayClaimName(staleRef),
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			Labels: map[string]string{
				"maas.opendatahub.io/gateway-claim": "true",
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      "team-cleanup-spoof",
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "AITenant",
				Name:       "team-real-owner",
				UID:        "uid-real-owner",
				Controller: &isController,
			}},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, spoofedStaleClaim, existingAITenantGateway("new-gw")).
		Build()
	r := &AITenantReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	currentRef := maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "new-gw"}
	err := r.cleanupStaleClaims(ctx, aitenant, currentRef)
	g.Expect(err).NotTo(HaveOccurred())

	// The spoofed stale claim must NOT have been deleted because its
	// OwnerReference points to a different AITenant.
	var remaining corev1.ConfigMap
	err = cl.Get(ctx, client.ObjectKey{
		Namespace: tenantreconcile.DefaultAITenantNamespace,
		Name:      gatewayClaimName(staleRef),
	}, &remaining)
	g.Expect(err).NotTo(HaveOccurred(), "spoofed stale claim should survive cleanupStaleClaims")
}
