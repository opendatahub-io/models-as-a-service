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
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func aigatewayTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(gatewayapiv1.Install(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func reconcileAIGatewayTwice(t *testing.T, r *AIGatewayReconciler, key types.NamespacedName) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(res).To(Equal(ctrl.Result{}))
}

func TestAIGatewayReconcile_CreatesBootstrapResources(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-a-maas"},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{
				Namespace:        "openshift-ingress",
				GatewayClassName: "openshift-default",
			},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://issuer.example.com/realms/team-a",
				ClientID:  "team-a-client",
			},
			RBAC: &maasv1alpha1.AIGatewayRBACConfig{
				Admins: []maasv1alpha1.AIGatewayRBACSubject{{
					Kind: rbacv1.GroupKind,
					Name: "team-a-admins",
				}},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var ns corev1.Namespace
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-a-maas"}, &ns)).To(Succeed())
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aigatewayCreatedAnnotation, "true"))
	g.Expect(ns.Annotations).To(HaveKeyWithValue(aigatewayNameAnnotation, "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("opendatahub.io/generated-namespace", "true"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-name", "team-a"))
	g.Expect(ns.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-namespace", "team-a-maas"))

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-a", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Spec.GatewayClassName).To(Equal(gatewayapiv1.ObjectName("openshift-default")))
	g.Expect(gateway.Spec.Listeners).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[0].Protocol).To(Equal(gatewayapiv1.HTTPProtocolType))
	g.Expect(gateway.Labels).To(HaveKeyWithValue("maas.opendatahub.io/tenant-name", "team-a"))

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-a-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.GatewayRef).To(Equal(maasv1alpha1.TenantGatewayRef{
		Namespace: "openshift-ingress",
		Name:      "team-a",
	}))
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://issuer.example.com/realms/team-a"))
	g.Expect(tenant.Spec.ExternalOIDC.ClientID).To(Equal("team-a-client"))

	var tenantRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aigw), Namespace: "team-a-maas"}, &tenantRole)).To(Succeed())
	g.Expect(tenantRole.Rules).NotTo(BeEmpty())
	for _, rule := range tenantRole.Rules {
		g.Expect(rule.Verbs).NotTo(ContainElement("*"))
		g.Expect(rule.Resources).NotTo(ContainElement("*"))
		g.Expect(rule.Verbs).NotTo(ContainElement("escalate"))
		g.Expect(rule.Verbs).NotTo(ContainElement("bind"))
		g.Expect(rule.Verbs).NotTo(ContainElement("impersonate"))
	}

	var tenantBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenantAdminRoleName(aigw), Namespace: "team-a-maas"}, &tenantBinding)).To(Succeed())
	g.Expect(tenantBinding.Subjects).To(ContainElement(rbacv1.Subject{
		Kind:     rbacv1.GroupKind,
		APIGroup: rbacv1.GroupName,
		Name:     "team-a-admins",
	}))

	var aigwRole rbacv1.Role
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aigatewayAccessRoleName(aigw), Namespace: "ai-gateway-system"}, &aigwRole)).To(Succeed())
	g.Expect(aigwRole.Rules).NotTo(BeEmpty())
	for _, rule := range aigwRole.Rules {
		g.Expect(rule.Verbs).NotTo(ContainElement("*"))
		g.Expect(rule.Resources).NotTo(ContainElement("*"))
		g.Expect(rule.Verbs).NotTo(ContainElement("escalate"))
		g.Expect(rule.Verbs).NotTo(ContainElement("bind"))
		g.Expect(rule.Verbs).NotTo(ContainElement("impersonate"))
	}

	var aigwBinding rbacv1.RoleBinding
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: aigatewayAccessRoleName(aigw), Namespace: "ai-gateway-system"}, &aigwBinding)).To(Succeed())
	g.Expect(aigwBinding.RoleRef.Name).To(Equal(aigatewayAccessRoleName(aigw)))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(ready.Reason).To(Equal("Reconciled"))
}

func TestAIGatewayReconcile_PreExistingNamespaceWithCreateFalse(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	create := false
	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-b",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{
				Name:   "team-b-maas",
				Create: &create,
			},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b-maas"}}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var updatedNS corev1.Namespace
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-b-maas"}, &updatedNS)).To(Succeed())
	g.Expect(updatedNS.Annotations).To(HaveKeyWithValue(aigatewayNameAnnotation, "team-b"))
	g.Expect(updatedNS.Annotations).NotTo(HaveKey(aigatewayCreatedAnnotation))
}

func TestAIGatewayReconcile_CreateFalseMissingNamespaceSetsPending(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	create := false
	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-c",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{
				Name:   "team-c-maas",
				Create: &create,
			},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Pending"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("TenantNamespaceMissing"))

	var gateway gatewayapiv1.Gateway
	err = cl.Get(context.Background(), client.ObjectKey{Name: "team-c", Namespace: "openshift-ingress"}, &gateway)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

func TestAIGatewayReconcile_RejectsProtectedNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-d",
			Namespace: "opendatahub",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-d-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
}

func TestAIGatewayReconcile_DeletionCleansUpChildren(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)
	ctx := context.Background()

	cleanup := true
	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-del",
			Namespace:  "ai-gateway-system",
			Finalizers: []string{aigatewayFinalizer},
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{
				Name:            "team-del-maas",
				CleanupOnDelete: &cleanup,
			},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-del-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
				aigatewayCreatedAnnotation:   "true",
			},
			Labels: map[string]string{aigatewayManagedLabel: "true"},
		},
	}
	gw := &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-del",
			Namespace: "openshift-ingress",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aigw),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aigw),
			Namespace: "team-del-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: tenantAdminRoleName(aigw)},
	}
	objRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aigatewayAccessRoleName(aigw),
			Namespace: "ai-gateway-system",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	objBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aigatewayAccessRoleName(aigw),
			Namespace: "ai-gateway-system",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-del",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: aigatewayAccessRoleName(aigw)},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns, gw, tenant, role, binding, objRole, objBinding).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	g.Expect(cl.Delete(ctx, aigw)).To(Succeed())

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "openshift-ingress", Name: "team-del"}, &gatewayapiv1.Gateway{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: maasv1alpha1.TenantInstanceName}, &maasv1alpha1.Tenant{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aigw)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "team-del-maas", Name: tenantAdminRoleName(aigw)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-gateway-system", Name: aigatewayAccessRoleName(aigw)}, &rbacv1.Role{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Namespace: "ai-gateway-system", Name: aigatewayAccessRoleName(aigw)}, &rbacv1.RoleBinding{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKey{Name: "team-del-maas"}, &corev1.Namespace{}))).To(BeTrue())

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, key, &maasv1alpha1.AIGateway{}))).To(BeTrue())
}

func TestAIGatewayReconcile_DeletionSkipsNamespaceWhenCleanupDisabled(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)
	ctx := context.Background()

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-noclean",
			Namespace:  "ai-gateway-system",
			Finalizers: []string{aigatewayFinalizer},
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-noclean-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-noclean-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-noclean",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
				aigatewayCreatedAnnotation:   "true",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	g.Expect(cl.Delete(ctx, aigw)).To(Succeed())

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())

	var surviving corev1.Namespace
	g.Expect(cl.Get(ctx, client.ObjectKey{Name: "team-noclean-maas"}, &surviving)).To(Succeed())
}

func TestAIGatewayReconcile_DeletionSkipsNamespaceNotCreatedByController(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)
	ctx := context.Background()

	cleanup := true
	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "team-preexist",
			Namespace:  "ai-gateway-system",
			Finalizers: []string{aigatewayFinalizer},
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{
				Name:            "team-preexist-maas",
				CleanupOnDelete: &cleanup,
			},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "team-preexist-maas",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "team-preexist",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	g.Expect(cl.Delete(ctx, aigw)).To(Succeed())

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())

	var surviving corev1.Namespace
	g.Expect(cl.Get(ctx, client.ObjectKey{Name: "team-preexist-maas"}, &surviving)).To(Succeed())
}

func TestAIGatewayReconcile_AdoptsPreExistingGateway(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adopt",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-adopt-maas"},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{
				Name:      "existing-gw",
				Namespace: "openshift-ingress",
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
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, preExistingGW).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "existing-gw", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Annotations).To(HaveKeyWithValue(aigatewayNameAnnotation, "team-adopt"))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Active"))
	g.Expect(updated.Status.GatewayRef.Name).To(Equal("existing-gw"))
}

func TestAIGatewayReconcile_AdoptsPreExistingTenant(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-adoptcfg",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-adoptcfg-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
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
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns, preExistingTenant).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-adoptcfg-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Annotations).To(HaveKeyWithValue(aigatewayNameAnnotation, "team-adoptcfg"))
	g.Expect(tenant.Spec.GatewayRef.Namespace).To(Equal("openshift-ingress"))
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://issuer.example.com/realms/adoptcfg"))
}

func TestAIGatewayReconcile_RejectsNamespaceOwnedByAnotherAIGateway(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-clash",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "shared-ns"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-ns",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "other-aigw",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, ns).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("TenantNamespaceFailed"))
	g.Expect(ready.Message).To(ContainSubstring("another AIGateway"))
}

func TestAIGatewayReconcile_RejectsGatewayOwnedByAnotherAIGateway(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-gwclash",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-gwclash-maas"},
			Gateway: &maasv1alpha1.AIGatewayGatewayTemplate{
				Name:      "contested-gw",
				Namespace: "openshift-ingress",
			},
		},
	}
	contestedGW := &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "contested-gw",
			Namespace: "openshift-ingress",
			Annotations: map[string]string{
				aigatewayNameAnnotation:      "other-aigw",
				aigatewayNamespaceAnnotation: "ai-gateway-system",
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw, contestedGW).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.Requeue).To(BeTrue())

	res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(30 * time.Second))

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("GatewayReconcileFailed"))
	g.Expect(ready.Message).To(ContainSubstring("another AIGateway"))
}

func TestAIGatewayReconcile_IdempotentOnSecondRun(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)
	ctx := context.Background()

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-idem",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-idem-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var afterFirst maasv1alpha1.AIGateway
	g.Expect(cl.Get(ctx, key, &afterFirst)).To(Succeed())
	g.Expect(afterFirst.Status.Phase).To(Equal("Active"))

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var afterSecond maasv1alpha1.AIGateway
	g.Expect(cl.Get(ctx, key, &afterSecond)).To(Succeed())
	g.Expect(afterSecond.Status.Phase).To(Equal("Active"))
	g.Expect(afterSecond.ResourceVersion).To(Equal(afterFirst.ResourceVersion))
}

func TestAIGatewayReconcile_OIDCFullMirror(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-oidc",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-oidc-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://sso.corp.example.com/realms/ai",
				ClientID:  "ai-platform",
				TTL:       600,
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-oidc-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.ExternalOIDC).NotTo(BeNil())
	g.Expect(tenant.Spec.ExternalOIDC.IssuerURL).To(Equal("https://sso.corp.example.com/realms/ai"))
	g.Expect(tenant.Spec.ExternalOIDC.ClientID).To(Equal("ai-platform"))
	g.Expect(tenant.Spec.ExternalOIDC.TTL).To(Equal(600))
}

func TestAIGatewayReconcile_NoOIDCSetsTenantOIDCNil(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-nooidc",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-nooidc-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var tenant maasv1alpha1.Tenant
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: "team-nooidc-maas"}, &tenant)).To(Succeed())
	g.Expect(tenant.Spec.ExternalOIDC).To(BeNil())
}

func TestAIGatewayReconcile_DomainCreatesHTTPListenerWithHostname(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-domain",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-domain-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
			Domain:          "team-domain.ai-gateway.apps.example.com",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-domain", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Spec.Listeners).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[0].Name).To(Equal(gatewayapiv1.SectionName("http")))
	g.Expect(gateway.Spec.Listeners[0].Port).To(Equal(gatewayapiv1.PortNumber(80)))
	g.Expect(gateway.Spec.Listeners[0].Protocol).To(Equal(gatewayapiv1.HTTPProtocolType))
	g.Expect(gateway.Spec.Listeners[0].Hostname).NotTo(BeNil())
	g.Expect(string(*gateway.Spec.Listeners[0].Hostname)).To(Equal("team-domain.ai-gateway.apps.example.com"))
	g.Expect(gateway.Spec.Listeners[0].TLS).To(BeNil())
}

func TestAIGatewayReconcile_DomainWithTLSCreatesHTTPSListener(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-tls",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "team-tls-maas"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
			Domain:          "team-tls.ai-gateway.apps.example.com",
			TLS: &maasv1alpha1.AIGatewayTLS{
				CertificateRef: maasv1alpha1.AIGatewayCertificateRef{Name: "team-tls-cert"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var gateway gatewayapiv1.Gateway
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "team-tls", Namespace: "openshift-ingress"}, &gateway)).To(Succeed())
	g.Expect(gateway.Spec.Listeners).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[0].Name).To(Equal(gatewayapiv1.SectionName("https")))
	g.Expect(gateway.Spec.Listeners[0].Port).To(Equal(gatewayapiv1.PortNumber(443)))
	g.Expect(gateway.Spec.Listeners[0].Protocol).To(Equal(gatewayapiv1.HTTPSProtocolType))
	g.Expect(gateway.Spec.Listeners[0].Hostname).NotTo(BeNil())
	g.Expect(string(*gateway.Spec.Listeners[0].Hostname)).To(Equal("team-tls.ai-gateway.apps.example.com"))
	g.Expect(gateway.Spec.Listeners[0].TLS).NotTo(BeNil())
	g.Expect(gateway.Spec.Listeners[0].TLS.CertificateRefs).To(HaveLen(1))
	g.Expect(gateway.Spec.Listeners[0].TLS.CertificateRefs[0].Name).To(Equal(gatewayapiv1.ObjectName("team-tls-cert")))
}

func TestAIGatewayChildName_Truncation(t *testing.T) {
	g := NewWithT(t)

	short := aigatewayChildName("team-a", "tenant-admin")
	g.Expect(short).To(Equal("aigateway-team-a-tenant-admin"))
	g.Expect(len(short)).To(BeNumerically("<=", 63))

	longName := "this-is-a-very-long-aigateway-name-that-exceeds-the-k8s-limit"
	truncated := aigatewayChildName(longName, "tenant-admin")
	g.Expect(len(truncated)).To(BeNumerically("<=", 63))
	g.Expect(truncated).To(HavePrefix("aigateway-"))
	g.Expect(truncated).To(ContainSubstring("tenant-admin"))

	g.Expect(aigatewayChildName(longName, "tenant-admin")).To(Equal(truncated))
}

func TestAIGatewayReconcile_RejectsTenantNamespaceEqualToAIGatewayNamespace(t *testing.T) {
	g := NewWithT(t)
	s := aigatewayTestScheme(t)

	aigw := &maasv1alpha1.AIGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-samens",
			Namespace: "ai-gateway-system",
		},
		Spec: maasv1alpha1.AIGatewaySpec{
			TenantNamespace: maasv1alpha1.AIGatewayTenantNamespace{Name: "ai-gateway-system"},
			Gateway:         &maasv1alpha1.AIGatewayGatewayTemplate{Namespace: "openshift-ingress"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AIGateway{}).
		WithObjects(aigw).
		Build()
	r := &AIGatewayReconciler{
		Client:           cl,
		Scheme:           s,
		APIReader:        cl,
		AppNamespace:     "opendatahub",
		TenantNamespace:  "models-as-a-service",
		GatewayNamespace: "openshift-ingress",
	}

	key := types.NamespacedName{Name: aigw.Name, Namespace: aigw.Namespace}
	reconcileAIGatewayTwice(t, r, key)

	var updated maasv1alpha1.AIGateway
	g.Expect(cl.Get(context.Background(), key, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, maasv1alpha1.AIGatewayConditionReady)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidPlacement"))
}
