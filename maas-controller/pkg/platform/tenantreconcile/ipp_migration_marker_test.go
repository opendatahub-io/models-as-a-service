package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestLegacyIPPBundleExists(t *testing.T) {
	const (
		gwNS     = "openshift-ingress"
		tenantID = "team-a"
	)
	scheme := praxisTestScheme(t)

	t.Run("absent when no Deployment exists", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		got, err := legacyIPPBundleExists(context.Background(), cl, PlatformParams{GatewayNamespace: gwNS, TenantIdentifier: tenantID})
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("present when Deployment exists", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingDeploymentName(tenantID),
				Namespace: gwNS,
			},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
		got, err := legacyIPPBundleExists(context.Background(), cl, PlatformParams{GatewayNamespace: gwNS, TenantIdentifier: tenantID})
		require.NoError(t, err)
		assert.True(t, got)
	})
}

func TestClaimIPPMigrationMarker(t *testing.T) {
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

	// TestClaimIPPMigrationMarker/absent_marker_is_blocked covers both the
	// "cleanup genuinely in flight" case and the "tenant has never swapped
	// backends before" case: absent is the marker's blocked resting state in
	// both. A brand-new tenant is seeded with IPPMigrationMarkerClearValue at
	// MaasTenantConfig creation time (see
	// AITenantReconciler.seedIPPMigrationCleanupCompleteOnCreate) precisely so
	// it never hits this path; this fixture models a tenant reconciled
	// without going through that seeding (e.g. a pre-existing tenant, or a
	// unit test that doesn't exercise AITenantReconciler).
	t.Run("absent marker is blocked", func(t *testing.T) {
		tenant := newTenant("ns-absent", nil)
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

		claimed, err := claimIPPMigrationMarker(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.False(t, claimed, "an absent marker must block a transitioning-in party, not permit it")
	})

	t.Run("cleared marker is claimable and claiming deletes it", func(t *testing.T) {
		tenant := newTenant("ns-clear", map[string]string{AnnotationIPPMigrationCleanupComplete: IPPMigrationMarkerClearValue})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

		claimed, err := claimIPPMigrationMarker(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.True(t, claimed)

		got := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-clear", Name: maasv1alpha1.MaasTenantConfigInstanceName}, got))
		_, stillPresent := got.Annotations[AnnotationIPPMigrationCleanupComplete]
		assert.False(t, stillPresent, "claiming must delete the annotation (return it to its blocked resting state), not write a sentinel value")
	})

	t.Run("unexpected non-clear value is also blocked", func(t *testing.T) {
		tenant := newTenant("ns-unexpected", map[string]string{AnnotationIPPMigrationCleanupComplete: "some-other-value"})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

		claimed, err := claimIPPMigrationMarker(context.Background(), cl, tenant)
		require.NoError(t, err)
		assert.False(t, claimed, "only the exact IPPMigrationMarkerClearValue may be claimed")
	})

	t.Run("missing tenant config surfaces an error, not a false claim", func(t *testing.T) {
		tenant := newTenant("ns-missing", nil)
		cl := fake.NewClientBuilder().WithScheme(scheme).Build() // tenant not seeded

		claimed, err := claimIPPMigrationMarker(context.Background(), cl, tenant)
		require.Error(t, err)
		assert.False(t, claimed)
	})

	// TestClaimIPPMigrationMarker/concurrent_claim_attempts simulates the
	// exact race this mechanism exists to close: two independent readers
	// (standing in for maas-controller and ai-gateway-controller) both
	// observe the marker as "clear to deploy" from the same object revision,
	// and both attempt to claim it. Exactly one of the two concurrent
	// Updates must win; the other must observe a Conflict (surfaced here as
	// claimed=false, err=nil) and back off rather than also proceeding to
	// deploy.
	t.Run("concurrent claim attempts: exactly one wins", func(t *testing.T) {
		tenant := newTenant("ns-flip-flop", map[string]string{AnnotationIPPMigrationCleanupComplete: IPPMigrationMarkerClearValue})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

		// Both "controllers" read the same object independently before either claims it.
		readerA := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-flip-flop", Name: maasv1alpha1.MaasTenantConfigInstanceName}, readerA))
		readerB := &maasv1alpha1.MaasTenantConfig{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "ns-flip-flop", Name: maasv1alpha1.MaasTenantConfigInstanceName}, readerB))

		claimedA, errA := claimIPPMigrationMarker(context.Background(), cl, readerA)
		require.NoError(t, errA)

		claimedB, errB := claimIPPMigrationMarker(context.Background(), cl, readerB)
		require.NoError(t, errB)

		assert.NotEqual(t, claimedA, claimedB, "exactly one of the two concurrent claim attempts must win")
		assert.True(t, claimedA || claimedB, "at least one attempt must win, or the tenant would be stuck forever")
	})
}
