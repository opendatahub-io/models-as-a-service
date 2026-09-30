//nolint:testpackage
package maas

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

var (
	testTenantGatewayName      = "maas-default-gateway"
	testTenantGatewayNamespace = "openshift-ingress"
)

func tenantTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(maasv1alpha1.AddToScheme(s))
	return s
}

func tenantTestNamespace(name string) client.Object {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func tenantTestUnstructured(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func TestTenantReconcile_DeletionIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         testNS,
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"example.com/hold"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: testNS}}
	res, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement("example.com/hold"), "Tenant reconciler does not mutate finalizers on delete")
}

func TestTenantReconcile_NonSingletonNameIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-default-tenant",
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "not-default-tenant", Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: "not-default-tenant", Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty(), "non-singleton should not get a finalizer")
}

func TestTenantReconcile_DefaultTenantDoesNotAddCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty(), "default-tenant teardown is Config-driven; no tenant-cleanup finalizer")
}

func TestTenantReconcile_AITenantManagedDefaultAddsCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   testNS,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement(tenantFinalizer))
}

func TestTenantReconcile_DefaultTenantStripsLegacyCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:       maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:  testNS,
			Finalizers: []string{"maas.opendatahub.io/tenant-cleanup"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(BeEmpty())
}

func TestTenantReconcile_AITenantManagedAddsCleanupFinalizer(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "ai-tenant-redteam"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "redteam",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:                          cl,
		Scheme:                          s,
		AppNamespace:                    "opendatahub",
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
		GatewayName:                     testTenantGatewayName,
		GatewayNamespace:                testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Finalizers).To(ContainElement("maas.opendatahub.io/tenant-cleanup"))
}

func TestTenantReconcile_AITenantManagedDefaultDeletionCleansPlatformResources(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	ctx := context.Background()
	now := metav1.NewTime(time.Now())

	const tenantNS = "models-as-a-service"
	const appNS = "odh-ai-gateway-infra"
	const gatewayNS = "openshift-ingress"

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         tenantNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   tenantNS,
			},
		},
	}

	resources := []client.Object{
		tenant,
		tenantTestUnstructured(tenantreconcile.GVKDeployment, appNS, tenantreconcile.MaaSAPIDeploymentName("")),
		tenantTestUnstructured(tenantreconcile.GVKService, appNS, tenantreconcile.MaaSAPIServiceName("")),
		tenantTestUnstructured(tenantreconcile.GVKHTTPRoute, appNS, tenantreconcile.MaaSAPIRouteName("")),
		tenantTestUnstructured(tenantreconcile.GVKCronJob, appNS, tenantreconcile.MaaSAPIKeyCleanupCronJobName("")),
		tenantTestUnstructured(tenantreconcile.GVKTokenRateLimitPolicy, gatewayNS, tenantreconcile.GatewayTokenRateLimitDefaultDenyPolicyName("")),
		tenantTestUnstructured(tenantreconcile.GVKDestinationRule, gatewayNS, tenantreconcile.GatewayDestinationRuleName("")),
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(resources...).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     appNS,
		TenantNamespace:  tenantNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: gatewayNS,
	}

	res, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	for _, obj := range resources[1:] {
		err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected %s %s/%s to be deleted", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
	}

	var updated maasv1alpha1.MaasTenantConfig
	err = cl.Get(ctx, client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}, &updated)
	if err == nil {
		g.Expect(updated.Finalizers).NotTo(ContainElement(tenantFinalizer))
	} else {
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
}

func TestTenantReconcile_AITenantManagedDefaultDeletionWaitsForMaaSCRFinalizers(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	ctx := context.Background()
	now := metav1.NewTime(time.Now())

	const tenantNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         tenantNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   tenantNS,
			},
		},
	}
	subscription := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "default-subscription",
			Namespace:  tenantNS,
			Finalizers: []string{maasSubscriptionFinalizer},
		},
	}
	policy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "default-auth-policy",
			Namespace:  tenantNS,
			Finalizers: []string{maasAuthPolicyFinalizer},
		},
	}
	deployment := tenantTestUnstructured(
		tenantreconcile.GVKDeployment,
		"odh-ai-gateway-infra",
		tenantreconcile.MaaSAPIDeploymentName(""),
	)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, subscription, policy, deployment).
		Build()
	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     "odh-ai-gateway-infra",
		TenantNamespace:  tenantNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}
	key := types.NamespacedName{Name: tenant.Name, Namespace: tenant.Namespace}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	var deletingSubscription maasv1alpha1.MaaSSubscription
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(subscription), &deletingSubscription)).To(Succeed())
	g.Expect(deletingSubscription.DeletionTimestamp.IsZero()).To(BeFalse())
	var deletingPolicy maasv1alpha1.MaaSAuthPolicy
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(policy), &deletingPolicy)).To(Succeed())
	g.Expect(deletingPolicy.DeletionTimestamp.IsZero()).To(BeFalse())
	var deletingTenant maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, key, &deletingTenant)).To(Succeed())
	g.Expect(deletingTenant.Finalizers).To(ContainElement(tenantFinalizer))
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(deployment), deployment)).To(Succeed())

	deletingSubscription.Finalizers = nil
	g.Expect(cl.Update(ctx, &deletingSubscription)).To(Succeed())
	deletingPolicy.Finalizers = nil
	g.Expect(cl.Update(ctx, &deletingPolicy)).To(Succeed())

	res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKeyFromObject(deployment), deployment))).To(BeTrue())
	err = cl.Get(ctx, key, &deletingTenant)
	if err == nil {
		g.Expect(deletingTenant.Finalizers).NotTo(ContainElement(tenantFinalizer))
	} else {
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
}

func TestTenantReconcile_ManagementStateRemovedWaitsForConfigTeardown(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  types.UID("ct-uid"),
		},
	}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var ctAfter maasv1alpha1.Config
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &ctAfter)).To(Succeed())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())

	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(readyCond.Reason).To(Equal("WaitingForRemovedTeardown"))
}

func TestTenantReconcile_ManagementStateRemoved_ConfigTerminatingPatchesStatus(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.ConfigInstanceName,
			UID:               types.UID("ct-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test/finalizer"},
		},
	}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateRemoved,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ConfigTerminating"))
}

func TestTenantReconcile_ManagementStateUnmanagedSetsIdle(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: managementStateUnmanaged,
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("ManagementStateIdle"))
}

func TestTenantReconcile_UnexpectedManagementStateSetsFailedPhase(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
			Annotations: map[string]string{
				managementStateAnnotation: "InvalidState",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS}, &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	g.Expect(updated.Status.InfraNamespace).To(Equal(testNS), "infraNamespace should be set even on error paths")
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(readyCond).NotTo(BeNil())
	g.Expect(readyCond.Reason).To(Equal("UnexpectedManagementState"))
}

func TestTenantReconcile_ConfigMissingSkipsPlatform(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigMissing"))
	g.Expect(updated.Status.InfraNamespace).To(Equal(testNS))
}

func TestTenantReconcile_InfraNamespaceSetInStatus(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const tenantNS = "models-as-a-service"
	const infraNS = "odh-ai-gateway-infra"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, tenantTestNamespace(tenantNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     infraNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS},
	})
	g.Expect(err).NotTo(HaveOccurred())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}, &updated)).To(Succeed())
	g.Expect(updated.Status.InfraNamespace).To(Equal(infraNS), "status.infraNamespace should reflect the separated infrastructure namespace")
}

func TestTenantReconcile_ConfigEmptyUIDPatchesWaitingForConfigUID(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.ConfigInstanceName,
			UID:  "",
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("WaitingForConfigUID"))
}

func TestTenantReconcile_ConfigTerminatingSkipsPlatform(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const testNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: testNS,
		},
	}
	ct := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.ConfigInstanceName,
			UID:               types.UID("ct-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test-finalizer"},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant, ct, tenantTestNamespace(testNS)).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     testNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: testNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(t.Context(), client.ObjectKey{Name: tenant.Name, Namespace: testNS}, &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ConfigTerminating"))
}

const platformFixtureNS = "models-as-a-service"

// newPlatformTenantFixture returns a TenantReconciler over a fake client holding the
// default MaasTenantConfig, Config/default with a UID, the fallback Gateway and extra,
// so a reconcile gets past the Config and Gateway gates. customize may add client
// options such as interceptors or a REST mapper.
func newPlatformTenantFixture(
	t *testing.T,
	customize func(*fake.ClientBuilder) *fake.ClientBuilder,
	extra ...client.Object,
) (*TenantReconciler, *maasv1alpha1.MaasTenantConfig) {
	t.Helper()

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: platformFixtureNS,
		},
	}
	objs := append([]client.Object{
		tenant,
		&maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("ct-uid")}},
		&gatewayapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: testTenantGatewayName, Namespace: testTenantGatewayNamespace}},
		tenantTestNamespace(platformFixtureNS),
	}, extra...)

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(objs...)
	if customize != nil {
		builder = customize(builder)
	}

	return &TenantReconciler{
		Client:           builder.Build(),
		Scheme:           scheme,
		AppNamespace:     platformFixtureNS,
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}, tenant
}

func TestTenantReconcile_UsageLogsEnvoyFilterDeleteErrorIsReturned(t *testing.T) {
	g := NewWithT(t)
	deleteErr := errors.New("envoyfilter delete refused")
	r, tenant := newPlatformTenantFixture(t, func(b *fake.ClientBuilder) *fake.ClientBuilder {
		return b.WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetObjectKind().GroupVersionKind() == tenantreconcile.GVKEnvoyFilter {
					return deleteErr
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
	})

	markTenantReady(t, r, tenant)

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	g.Expect(err).To(MatchError(deleteErr))
	g.Expect(res).To(Equal(ctrl.Result{}))

	// A failure that retries shows as Degraded; Ready stays, so the AITenant and
	// Lifecycle watches do not see readiness flap on a transient error.
	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	g.Expect(apimeta.IsStatusConditionTrue(updated.Status.Conditions, tenantreconcile.ReadyConditionType)).To(BeTrue())
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Reason).To(Equal("UsageLogsCleanupFailed"))
	g.Expect(degraded.Message).To(ContainSubstring(deleteErr.Error()))
}

// markTenantReady records a completed platform pass on the fixture tenant config.
func markTenantReady(t *testing.T, r *TenantReconciler, tenant *maasv1alpha1.MaasTenantConfig) {
	t.Helper()

	g := NewWithT(t)
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), tenant)).To(Succeed())
	tenant.Status.Phase = "Active"
	apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionTrue, Reason: "Reconciled",
		ObservedGeneration: tenant.Generation,
	})
	g.Expect(r.Status().Update(t.Context(), tenant)).To(Succeed())
}

func TestTenantReconcile_RecordFailureKeepsStatusWriteErrorRetryable(t *testing.T) {
	g := NewWithT(t)
	conflict := apierrors.NewConflict(maasv1alpha1.GroupVersion.WithResource("maastenantconfigs").GroupResource(),
		maasv1alpha1.MaasTenantConfigInstanceName, errors.New("object was modified"))
	r, tenant := newPlatformTenantFixture(t, func(b *fake.ClientBuilder) *fake.ClientBuilder {
		return b.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return conflict
			},
		})
	})

	err := r.recordFailure(t.Context(), tenant, "PlatformReconcileFailed", reconcile.TerminalError(errors.New("kustomize: missing overlay")))

	g.Expect(apierrors.IsConflict(err)).To(BeTrue())
	g.Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeFalse())
	g.Expect(err).To(MatchError(ContainSubstring("kustomize: missing overlay")))
}

// markAITenantManaged turns the platform fixture tenant config into one AITenant aitenantName
// owns, so the reconcile resolves its gateway from that AITenant.
func markAITenantManaged(t *testing.T, r *TenantReconciler, tenant *maasv1alpha1.MaasTenantConfig, aitenantName string) {
	t.Helper()

	g := NewWithT(t)
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), tenant)).To(Succeed())
	tenant.Labels = map[string]string{
		tenantreconcile.LabelManagedByAITenant: "true",
		tenantreconcile.LabelTenantName:        aitenantName,
		tenantreconcile.LabelTenantNamespace:   tenant.Namespace,
	}
	tenant.Annotations = map[string]string{
		tenantreconcile.AnnotationAITenantName:      aitenantName,
		tenantreconcile.AnnotationAITenantNamespace: tenant.Namespace,
	}
	g.Expect(r.Update(t.Context(), tenant)).To(Succeed())
}

func TestTenantReconcile_MissingAITenantWaitsForWatch(t *testing.T) {
	g := NewWithT(t)
	r, tenant := newPlatformTenantFixture(t, nil)
	markAITenantManaged(t, r, tenant, "team-a")

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidGateway"))
}

func TestTenantReconcile_AITenantReadFailureIsReturned(t *testing.T) {
	g := NewWithT(t)
	readErr := errors.New("connection refused")
	r, tenant := newPlatformTenantFixture(t, func(b *fake.ClientBuilder) *fake.ClientBuilder {
		return b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*maasv1alpha1.AITenant); ok {
					return readErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	})
	markAITenantManaged(t, r, tenant, "team-a")

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	g.Expect(err).To(MatchError(readErr))
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)).To(BeNil())
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Reason).To(Equal("PlatformContextReadFailed"))
}

func TestTenantReconcile_ManifestPathUnsetDoesNotRequeue(t *testing.T) {
	g := NewWithT(t)
	r, tenant := newPlatformTenantFixture(t, nil)

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("ManifestPathUnset"))
}

func TestTenantReconcile_PlatformReconcileErrorIsReturnedWithStatus(t *testing.T) {
	g := NewWithT(t)
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: platformFixtureNS},
		Data: map[string][]byte{
			tenantreconcile.MaaSDBSecretKey: []byte("postgresql://maas@db.example.com:5432/maas"),
		},
	}
	// CheckDependencies needs the AuthConfig API to resolve.
	mapper := apimeta.NewDefaultRESTMapper(nil)
	mapper.Add(tenantreconcile.GVKAuthConfig, apimeta.RESTScopeNamespace)
	r, tenant := newPlatformTenantFixture(t, func(b *fake.ClientBuilder) *fake.ClientBuilder {
		return b.WithRESTMapper(mapper)
	}, dbSecret)
	r.ManifestPath = filepath.Join(t.TempDir(), "missing-overlay")

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
	g.Expect(err).To(MatchError(ContainSubstring("tenant platform reconcile")))
	// A missing overlay stays missing until the process restarts, so retrying is pointless.
	g.Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeTrue())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	g.Expect(updated.Status.Phase).To(Equal("Failed"))
	// Its own reason tells a render that needs an edit from an apply that is retrying.
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("PlatformRenderFailed"))
	deployments := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionDeploymentsAvailable)
	g.Expect(deployments).NotTo(BeNil())
	g.Expect(deployments.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(deployments.Reason).To(Equal("PlatformRenderFailed"))
}

func TestTenantReconcile_AppNamespaceUsesConfiguredAppNamespace(t *testing.T) {
	g := NewWithT(t)
	r := &TenantReconciler{AppNamespace: "opendatahub"}
	g.Expect(r.appNamespaceForTenant()).To(Equal("opendatahub"))
}

func TestTenantReconcile_AppNamespaceReturnsRHOAINamespace(t *testing.T) {
	g := NewWithT(t)
	r := &TenantReconciler{AppNamespace: "redhat-ods-applications"}
	g.Expect(r.appNamespaceForTenant()).To(Equal("redhat-ods-applications"))
}

func TestTenantReconcile_NotFoundIsNoOp(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	r := &TenantReconciler{
		Client:           cl,
		Scheme:           s,
		AppNamespace:     "models-as-a-service",
		GatewayName:      testTenantGatewayName,
		GatewayNamespace: testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service"},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestTenantReconcile_TeardownRequestedSkipsReconciliation(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const controllerNS = "opendatahub"
	const tenantNS = "models-as-a-service"

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: controllerNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(dep, tenant).
		Build()

	r := &TenantReconciler{
		Client:              cl,
		Scheme:              s,
		ControllerNamespace: controllerNS,
		TenantNamespace:     tenantNS,
		AppNamespace:        tenantNS,
		GatewayName:         testTenantGatewayName,
		GatewayNamespace:    testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: tenantNS},
	})
	g.Expect(err).NotTo(HaveOccurred(), "should return immediately without error during teardown")
	g.Expect(res).To(Equal(ctrl.Result{}), "should return empty result (no requeue)")

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: tenantNS}, &updated)).To(Succeed())
	g.Expect(updated.Status.Conditions).To(BeEmpty(), "should not set any status conditions during teardown")
}

func TestTenantReconcile_TeardownRequestedStillHandlesDeletion(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const controllerNS = "opendatahub"
	const tenantNS = "models-as-a-service"
	now := metav1.NewTime(time.Now())

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-controller",
			Namespace: controllerNS,
			Annotations: map[string]string{
				TeardownRequestedAnnotation: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "maas-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "maas-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "test"}}},
			},
		},
	}

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         tenantNS,
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(dep, tenant).
		Build()

	r := &TenantReconciler{
		Client:              cl,
		Scheme:              s,
		ControllerNamespace: controllerNS,
		TenantNamespace:     tenantNS,
		AppNamespace:        tenantNS,
		GatewayName:         testTenantGatewayName,
		GatewayNamespace:    testTenantGatewayNamespace,
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: tenantNS},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	var updated maasv1alpha1.MaasTenantConfig
	err = cl.Get(context.Background(), client.ObjectKey{Name: tenant.Name, Namespace: tenantNS}, &updated)
	if apierrors.IsNotFound(err) {
		return
	}
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updated.Finalizers).NotTo(ContainElement(tenantFinalizer), "deletion cleanup should run during teardown")
}

func TestTenantReconcile_InvalidTenantIdentifierFailsAfterDeletionCheck(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)

	const tenantNS = "broken-tenant-ns"

	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenant).
		Build()

	r := &TenantReconciler{
		Client:                          cl,
		Scheme:                          s,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
		GatewayName:                     testTenantGatewayName,
		GatewayNamespace:                testTenantGatewayNamespace,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: tenant.Name, Namespace: tenantNS},
	})
	g.Expect(err).To(MatchError(ContainSubstring("tenant-name is missing")))
	// Only a label fix helps, and that passes the MaasTenantConfig watch.
	g.Expect(errors.Is(err, reconcile.TerminalError(nil))).To(BeTrue())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ReadyConditionType)
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Reason).To(Equal("InvalidTenantIdentity"))
}

func TestAggregateWarningsAndSetDegraded(t *testing.T) {
	tests := []struct {
		name             string
		prereqWarnings   []string
		replicaWarnings  []string
		kuadrantWarning  string
		usageLogsWarning string
		wantReason       string
		wantStatus       metav1.ConditionStatus
		wantMessage      string
	}{
		{
			name:            "no warnings",
			prereqWarnings:  nil,
			replicaWarnings: nil,
			wantReason:      "NoWarnings",
			wantStatus:      metav1.ConditionFalse,
			wantMessage:     "",
		},
		{
			name:            "single prerequisite warning",
			prereqWarnings:  []string{"DSCI monitoring stack not available"},
			replicaWarnings: nil,
			wantReason:      "PrerequisitesWarning",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "DSCI monitoring stack not available",
		},
		{
			name:            "multiple prerequisite warnings",
			prereqWarnings:  []string{"DSCI monitoring stack not available", "Perses not available"},
			replicaWarnings: nil,
			wantReason:      "PrerequisitesWarning",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "DSCI monitoring stack not available; Perses not available",
		},
		{
			name:            "single replica warning",
			prereqWarnings:  nil,
			replicaWarnings: []string{"invalid replica annotation on maas-api"},
			wantReason:      "InvalidReplicaAnnotation",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "invalid replica annotation on maas-api",
		},
		{
			name:            "multiple replica warnings",
			prereqWarnings:  nil,
			replicaWarnings: []string{"invalid replica annotation on maas-api", "invalid replica annotation on payload-processing"},
			wantReason:      "InvalidReplicaAnnotation",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "invalid replica annotation on maas-api; invalid replica annotation on payload-processing",
		},
		{
			name:            "both prerequisite and replica warnings",
			prereqWarnings:  []string{"DSCI monitoring stack not available"},
			replicaWarnings: []string{"invalid replica annotation on maas-api"},
			wantReason:      "MultipleWarnings",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "DSCI monitoring stack not available; invalid replica annotation on maas-api",
		},
		{
			name:            "multiple of both types",
			prereqWarnings:  []string{"DSCI monitoring stack not available", "Perses not available"},
			replicaWarnings: []string{"invalid replica annotation on maas-api", "invalid replica annotation on payload-processing"},
			wantReason:      "MultipleWarnings",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "DSCI monitoring stack not available; Perses not available; invalid replica annotation on maas-api; invalid replica annotation on payload-processing",
		},
		{
			name:             "usage logging warning only",
			usageLogsWarning: "Usage-logs EnvoyFilter not deployed: manifest or CRD not available",
			wantReason:       "UsageLoggingNotProvided",
			wantStatus:       metav1.ConditionTrue,
			wantMessage:      "Usage-logs EnvoyFilter not deployed: manifest or CRD not available",
		},
		{
			name:             "usage logging warning with prerequisite warning",
			prereqWarnings:   []string{"DSCI monitoring stack not available"},
			usageLogsWarning: "Usage-logs EnvoyFilter not deployed: manifest or CRD not available",
			wantReason:       "MultipleWarnings",
			wantStatus:       metav1.ConditionTrue,
			wantMessage:      "DSCI monitoring stack not available; Usage-logs EnvoyFilter not deployed: manifest or CRD not available",
		},
		{
			name:            "kuadrant detection warning only",
			kuadrantWarning: "cannot get WasmPlugin openshift-ingress/kuadrant-maas-default-gateway",
			wantReason:      "KuadrantDetectionUnverified",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "cannot get WasmPlugin openshift-ingress/kuadrant-maas-default-gateway",
		},
		{
			name:            "kuadrant detection warning with replica warning",
			replicaWarnings: []string{"invalid replica annotation on maas-api"},
			kuadrantWarning: "cannot get WasmPlugin openshift-ingress/kuadrant-maas-default-gateway",
			wantReason:      "MultipleWarnings",
			wantStatus:      metav1.ConditionTrue,
			wantMessage:     "invalid replica annotation on maas-api; cannot get WasmPlugin openshift-ingress/kuadrant-maas-default-gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			s := tenantTestScheme(t)

			tenant := &maasv1alpha1.MaasTenantConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "default-tenant",
					Namespace:  "models-as-a-service",
					Generation: 1,
				},
			}

			prereqReport := tenantreconcile.PrerequisiteReport{
				Warnings: tt.prereqWarnings,
			}

			var runRes *tenantreconcile.RunResult
			if tt.replicaWarnings != nil || tt.kuadrantWarning != "" {
				runRes = &tenantreconcile.RunResult{
					Warnings:                 tt.replicaWarnings,
					KuadrantDetectionWarning: tt.kuadrantWarning,
				}
			}

			r := &TenantReconciler{
				Client: fake.NewClientBuilder().WithScheme(s).Build(),
				Scheme: s,
			}

			r.aggregateWarningsAndSetDegraded(tenant, prereqReport, runRes, tt.usageLogsWarning)

			degradedCond := apimeta.FindStatusCondition(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
			g.Expect(degradedCond).NotTo(BeNil(), "Degraded condition should be set")
			g.Expect(degradedCond.Status).To(Equal(tt.wantStatus), "Degraded status mismatch")
			g.Expect(degradedCond.Reason).To(Equal(tt.wantReason), "Degraded reason mismatch")
			g.Expect(degradedCond.Message).To(Equal(tt.wantMessage), "Degraded message mismatch")
		})
	}
}
