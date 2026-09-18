package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestPayloadProcessingStatus(t *testing.T) {
	newTenant := func(annotations map[string]string) *maasv1alpha1.MaasTenantConfig {
		return &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	}
	assert.Equal(t, "", payloadProcessingStatus(newTenant(nil)))
	assert.Equal(t, PayloadProcessingStatusCleanupComplete, payloadProcessingStatus(newTenant(map[string]string{
		AnnotationPayloadProcessingStatus: PayloadProcessingStatusCleanupComplete,
	})))
	assert.True(t, isPayloadProcessingCleanupComplete(newTenant(map[string]string{
		AnnotationPayloadProcessingStatus: PayloadProcessingStatusCleanupComplete,
	})))
	assert.False(t, isPayloadProcessingCleanupComplete(newTenant(nil)))
}

func TestEnsureLegacyMayDeploy(t *testing.T) {
	scheme := praxisTestScheme(t)

	newTenant := func(namespace string, annotations map[string]string) *maasv1alpha1.MaasTenantConfig {
		return &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:        maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace:   namespace,
				Annotations: annotations,
			},
		}
	}

	t.Run("absent is ready", func(t *testing.T) {
		tenant := newTenant("ns-absent", nil)
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
		ready, err := ensureLegacyMayDeploy(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.True(t, ready)
	})

	t.Run("cleanup-complete claims to absent", func(t *testing.T) {
		tenant := newTenant("ns-clear", map[string]string{AnnotationPayloadProcessingStatus: PayloadProcessingStatusCleanupComplete})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
		ready, err := ensureLegacyMayDeploy(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.True(t, ready)

		got := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-clear", Name: maasv1alpha1.MaasTenantConfigInstanceName}, got))
		assert.Equal(t, "", payloadProcessingStatus(got))
	})

	t.Run("peer-owned value blocks legacy", func(t *testing.T) {
		// Opaque peer claim (ai-gateway-controller writes "steady"; maas must
		// not special-case that string — any non-absent/non-cleanup-complete
		// value blocks).
		tenant := newTenant("ns-peer", map[string]string{AnnotationPayloadProcessingStatus: "steady"})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
		ready, err := ensureLegacyMayDeploy(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.False(t, ready)
	})

	t.Run("concurrent claim attempts: exactly one wins", func(t *testing.T) {
		tenant := newTenant("ns-flip-flop", map[string]string{AnnotationPayloadProcessingStatus: PayloadProcessingStatusCleanupComplete})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

		readerA := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-flip-flop", Name: maasv1alpha1.MaasTenantConfigInstanceName}, readerA))
		readerB := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-flip-flop", Name: maasv1alpha1.MaasTenantConfigInstanceName}, readerB))

		claimedA, errA := claimLegacySteady(context.Background(), cl, readerA)
		require.NoError(t, errA)
		claimedB, errB := claimLegacySteady(context.Background(), cl, readerB)
		require.NoError(t, errB)

		assert.NotEqual(t, claimedA, claimedB)
		assert.True(t, claimedA || claimedB)
	})
}
