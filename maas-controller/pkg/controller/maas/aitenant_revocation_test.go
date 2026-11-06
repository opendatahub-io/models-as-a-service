package maas

import (
	"context"
	"testing"
	"time"

	batcv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func TestEnsureTenantAPIKeysRevoked_RetainsWhenExternalOwnerRequestsRetention(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()
	appNS := "odh-ai-gateway-infra"

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-retain",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("retain-uid"),
			Annotations: map[string]string{
				retainAPIKeysOnDeleteAnnotation: "true",
			},
		},
	}
	tenantConfig := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-" + aitenant.Name,
			Annotations: map[string]string{
				aitenantNameAnnotation:      aitenant.Name,
				aitenantNamespaceAnnotation: aitenant.Namespace,
			},
		},
	}
	recorder := events.NewFakeRecorder(1)
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}, &maasv1alpha1.MaasTenantConfig{}).
		WithObjects(aitenantRevocationOperandObjects(appNS, aitenant.Name, aitenant.DeepCopy(), tenantConfig)...).
		Build()
	r := &AITenantReconciler{
		Client:       cl,
		Scheme:       s,
		APIReader:    cl,
		AppNamespace: appNS,
		Recorder:     recorder,
	}

	revoked, err := r.ensureTenantAPIKeysRevoked(ctx, aitenant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(revoked).To(BeTrue())

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(aitenant), &updated)).To(Succeed())
	condition := apimeta.FindStatusCondition(updated.Status.Conditions, aitenantAPIKeysRevokedCondition)
	g.Expect(condition).NotTo(BeNil())
	g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(condition.Reason).To(Equal(aitenantAPIKeysRevokedReasonRetained))

	job := tenantAPIKeyRevocationJob(aitenant, appNS)
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKeyFromObject(job), &batcv1.Job{}))).To(BeTrue())

	var updatedTenantConfig maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(tenantConfig), &updatedTenantConfig)).To(Succeed())
	tenantConfigCondition := apimeta.FindStatusCondition(updatedTenantConfig.Status.Conditions, maasTenantConfigAPIKeysRetainedCondition)
	g.Expect(tenantConfigCondition).NotTo(BeNil())
	g.Expect(tenantConfigCondition.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(tenantConfigCondition.Reason).To(Equal(maasTenantConfigAPIKeysRetainedReason))
	g.Expect(tenantConfigCondition.Message).To(Equal(revocationOperandsRequestMarker(aitenant)))

	select {
	case event := <-recorder.Events:
		g.Expect(event).To(ContainSubstring("Normal APIKeyCleanupSkipped"))
	default:
		t.Fatal("expected APIKeyCleanupSkipped event")
	}
}

func TestAITenantDeletion_RetainsTenantAndSubscriptionKeysThroughFinalizers(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	const tenantNS = "models-as-a-service"
	const appNS = "odh-ai-gateway-infra"

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       tenantreconcile.DefaultAITenantName,
			Namespace:  tenantreconcile.DefaultAITenantNamespace,
			UID:        types.UID("retain-cascade-uid"),
			Finalizers: []string{aitenantFinalizer},
			Annotations: map[string]string{
				retainAPIKeysOnDeleteAnnotation: "true",
			},
		},
	}
	tenantConfig := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
				tenantreconcile.LabelTenantNamespace:   tenantNS,
			},
			Annotations: map[string]string{
				aitenantNameAnnotation:      tenantreconcile.DefaultAITenantName,
				aitenantNamespaceAnnotation: tenantreconcile.DefaultAITenantNamespace,
			},
		},
	}
	subscription := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "projected-subscription",
			Namespace:  tenantNS,
			Finalizers: []string{maasSubscriptionFinalizer},
		},
	}
	maasAPIDeployment := tenantTestUnstructured(tenantreconcile.GVKDeployment, appNS, tenantreconcile.MaaSAPIDeploymentName(""))
	maasAPIService := tenantTestUnstructured(tenantreconcile.GVKService, appNS, tenantreconcile.MaaSAPIServiceName(""))

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}, &maasv1alpha1.MaasTenantConfig{}, &maasv1alpha1.MaaSSubscription{}).
		WithObjects(aitenant, tenantConfig, subscription, maasAPIDeployment, maasAPIService).
		Build()

	aitenantReconciler := &AITenantReconciler{
		Client:          cl,
		Scheme:          s,
		APIReader:       cl,
		AppNamespace:    appNS,
		TenantNamespace: tenantNS,
	}
	tenantReconciler := &TenantReconciler{
		Client:          cl,
		Scheme:          s,
		AppNamespace:    appNS,
		TenantNamespace: tenantNS,
	}
	subscriptionReconciler := &MaaSSubscriptionReconciler{
		Client:       cl,
		Scheme:       s,
		APIReader:    cl,
		AppNamespace: appNS,
	}
	subscriptionReconciler.inferenceObjectivesUnavailable.Store(true)

	aitenantKey := types.NamespacedName{Name: aitenant.Name, Namespace: aitenant.Namespace}
	g.Expect(cl.Delete(ctx, aitenant)).To(Succeed())

	aitenantRequest := ctrl.Request{NamespacedName: aitenantKey}
	res, err := aitenantReconciler.Reconcile(ctx, aitenantRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	res, err = aitenantReconciler.Reconcile(ctx, aitenantRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	tenantKey := types.NamespacedName{Name: tenantConfig.Name, Namespace: tenantConfig.Namespace}
	res, err = tenantReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: tenantKey})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(5 * time.Second))

	subscriptionKey := types.NamespacedName{Name: subscription.Name, Namespace: subscription.Namespace}
	res, err = subscriptionReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: subscriptionKey})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	res, err = tenantReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: tenantKey})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	res, err = aitenantReconciler.Reconcile(ctx, aitenantRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	g.Expect(apierrors.IsNotFound(cl.Get(ctx, aitenantKey, &maasv1alpha1.AITenant{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, tenantKey, &maasv1alpha1.MaasTenantConfig{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, subscriptionKey, &maasv1alpha1.MaaSSubscription{}))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKeyFromObject(maasAPIDeployment), maasAPIDeployment))).To(BeTrue())
	g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKeyFromObject(maasAPIService), maasAPIService))).To(BeTrue())

	jobs := &batcv1.JobList{}
	g.Expect(cl.List(ctx, jobs)).To(Succeed())
	g.Expect(jobs.Items).To(BeEmpty())
}

func TestRevocationOperandsRequestMarker(t *testing.T) {
	g := NewWithT(t)
	deletionTime := metav1.NewTime(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			UID:               types.UID("tenant-uid"),
			DeletionTimestamp: &deletionTime,
		},
	}
	g.Expect(revocationOperandsRequestMarker(aitenant)).To(Equal("tenant-uid/2026-10-19T12:00:00Z"))
	g.Expect(revocationOperandsRequestMarker(&maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("tenant-uid")},
	})).To(Equal("tenant-uid"))
}

func TestClearTenantConfigAPIKeysRetained(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-recreated",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("recreated-uid"),
		},
	}
	tenantConfig := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-team-recreated",
			Annotations: map[string]string{
				aitenantNameAnnotation:      aitenant.Name,
				aitenantNamespaceAnnotation: aitenant.Namespace,
			},
		},
		Status: maasv1alpha1.MaasTenantConfigStatus{
			Conditions: []metav1.Condition{{
				Type:    maasTenantConfigAPIKeysRetainedCondition,
				Status:  metav1.ConditionTrue,
				Reason:  maasTenantConfigAPIKeysRetainedReason,
				Message: "old-uid/2026-11-06T12:00:00Z",
			}},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(tenantConfig).
		Build()
	r := &AITenantReconciler{Client: cl, APIReader: cl, Scheme: s}

	g.Expect(r.clearTenantConfigAPIKeysRetained(ctx, aitenant)).To(Succeed())

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(tenantConfig), &updated)).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(updated.Status.Conditions, maasTenantConfigAPIKeysRetainedCondition)).To(BeNil())
}

func TestRequestRevocationOperandsViaMaasTenantConfig_IdempotentPerDeletion(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("delete-uid"),
		},
	}
	marker := revocationOperandsRequestMarker(aitenant)
	tenantNS := tenantreconcile.DefaultAITenantName
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(aitenant, tenant).
		Build()
	r := &AITenantReconciler{
		Client:    cl,
		Scheme:    s,
		APIReader: cl,
	}

	g.Expect(r.requestRevocationOperandsViaMaasTenantConfig(ctx, aitenant)).To(Succeed())
	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	g.Expect(updated.Annotations[maasTenantConfigRevocationOperandsRequestedAnnotation]).To(Equal(marker))

	patchCount := 0
	intercepted := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(aitenant, updated.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCount++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r.Client = intercepted
	g.Expect(r.requestRevocationOperandsViaMaasTenantConfig(ctx, aitenant)).To(Succeed())
	g.Expect(patchCount).To(Equal(0))
}

func TestRequestRevocationOperandsViaMaasTenantConfig_NewDeletionUpdatesMarker(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()

	tenantNS := tenantreconcile.DefaultAITenantName
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantNS,
			Annotations: map[string]string{
				maasTenantConfigRevocationOperandsRequestedAnnotation: "old-uid/2026-10-18T12:00:00Z",
			},
		},
	}
	newDeletion := metav1.NewTime(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              tenantreconcile.DefaultAITenantName,
			Namespace:         tenantreconcile.DefaultAITenantNamespace,
			UID:               types.UID("new-uid"),
			DeletionTimestamp: &newDeletion,
			Finalizers:        []string{aitenantFinalizer},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(aitenant, tenant).
		Build()
	r := &AITenantReconciler{
		Client:    cl,
		Scheme:    s,
		APIReader: cl,
	}

	g.Expect(r.requestRevocationOperandsViaMaasTenantConfig(ctx, aitenant)).To(Succeed())
	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(tenant), &updated)).To(Succeed())
	g.Expect(updated.Annotations[maasTenantConfigRevocationOperandsRequestedAnnotation]).To(Equal(revocationOperandsRequestMarker(aitenant)))
}

func TestEnsureTenantAPIKeysRevoked_NudgesRecoveryWhenOperandsMissing(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()
	appNS := "odh-ai-gateway-infra"
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("recover-uid"),
		},
	}
	cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName}}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantreconcile.DefaultAITenantName,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, cfg, tenant).
		Build()
	r := &AITenantReconciler{
		Client:       cl,
		Scheme:       s,
		APIReader:    cl,
		AppNamespace: appNS,
	}

	revoked, err := r.ensureTenantAPIKeysRevoked(ctx, aitenant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(revoked).To(BeFalse())

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(aitenant), &updated)).To(Succeed())
	g.Expect(apimeta.FindStatusCondition(updated.Status.Conditions, aitenantAPIKeysRevokedCondition)).To(BeNil())

	var updatedTenant maasv1alpha1.MaasTenantConfig
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(tenant), &updatedTenant)).To(Succeed())
	g.Expect(updatedTenant.Annotations[maasTenantConfigRevocationOperandsRequestedAnnotation]).To(Equal(revocationOperandsRequestMarker(aitenant)))
}

func TestEnsureTenantAPIKeysRevoked_SkipsWhenMaasTenantConfigTerminating(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()
	appNS := "odh-ai-gateway-infra"

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("terminating-uid"),
		},
	}
	cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName}}
	// The AITenant and its MaasTenantConfig are removed together (Config owner-ref
	// cascade). A terminating MaasTenantConfig can never restore the operands, so the
	// AITenant must skip revocation instead of requeueing forever.
	deletion := metav1.NewTime(time.Date(2026, 10, 28, 1, 46, 41, 0, time.UTC))
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:              maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:         tenantreconcile.DefaultAITenantName,
			DeletionTimestamp: &deletion,
			Finalizers:        []string{tenantFinalizer},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, cfg, tenant).
		Build()
	r := &AITenantReconciler{
		Client:       cl,
		Scheme:       s,
		APIReader:    cl,
		AppNamespace: appNS,
		Recorder:     events.NewFakeRecorder(10),
	}

	revoked, err := r.ensureTenantAPIKeysRevoked(ctx, aitenant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(revoked).To(BeTrue())

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(aitenant), &updated)).To(Succeed())
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, aitenantAPIKeysRevokedCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(aitenantAPIKeysRevokedReasonOperandsUnavailable))
	g.Expect(cond.Message).To(ContainSubstring("terminating"))
}

func TestEnsureTenantAPIKeysRevoked_DoesNotSkipOnForbiddenServiceAccountGet(t *testing.T) {
	g := NewWithT(t)
	s := aitenantTestScheme(t)
	ctx := context.Background()
	appNS := "odh-ai-gateway-infra"

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: tenantreconcile.DefaultAITenantNamespace,
			UID:       types.UID("forbidden-uid"),
		},
	}
	cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName}}
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantreconcile.DefaultAITenantName,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		WithObjects(aitenant, cfg, tenant).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok && key.Name == aitenantAPIKeyCleanupServiceAccountName {
					return apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "serviceaccounts"}, key.Name, nil)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &AITenantReconciler{
		Client:       cl,
		Scheme:       s,
		APIReader:    cl,
		AppNamespace: appNS,
		Recorder:     events.NewFakeRecorder(1),
	}

	revoked, err := r.ensureTenantAPIKeysRevoked(ctx, aitenant)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(revoked).To(BeFalse())

	var updated maasv1alpha1.AITenant
	g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(aitenant), &updated)).To(Succeed())
	revokedCondition := apimeta.FindStatusCondition(updated.Status.Conditions, aitenantAPIKeysRevokedCondition)
	g.Expect(revokedCondition).To(BeNil())
}
