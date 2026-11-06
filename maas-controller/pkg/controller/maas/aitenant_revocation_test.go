package maas

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

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
