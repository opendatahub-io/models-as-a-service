package maas

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestSubscriptionRateLimitID_StableAndShort(t *testing.T) {
	key := ModelScopedSubscriptionKey("models-as-a-service", "demo08-hybrid-catalog", "llm", "granite-3-8b-instruct")
	id := SubscriptionRateLimitID(key)
	assert.Len(t, id, 16)
	assert.Regexp(t, `^[0-9a-f]{16}$`, id)
	assert.Equal(t, id, SubscriptionRateLimitID(key), "must be deterministic")
	assert.NotEqual(t, id, SubscriptionRateLimitID(key+"x"))
}

func TestSubscriptionRateLimitID_KnownVector(t *testing.T) {
	// Fixed vector shared with maas-api/internal/subscription RateLimitID.
	key := "models-as-a-service/demo-sub@llm/sim-chat"
	assert.Equal(t, "15d4c0904b3ebaf9", SubscriptionRateLimitID(key))
}

func TestClassifiedRateLimitIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sub  *maasv1alpha1.MaaSSubscription
		want string
	}{
		{name: "nil", want: RateLimitIdentityLegacy},
		{
			name: "new empty CR is short",
			sub:  &maasv1alpha1.MaaSSubscription{},
			want: RateLimitIdentityShort,
		},
		{
			name: "surviving CR with phase is legacy",
			sub: &maasv1alpha1.MaaSSubscription{
				Status: maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "surviving CR with conditions is legacy",
			sub: &maasv1alpha1.MaaSSubscription{
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
				},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "sticky status short wins over phase",
			sub: &maasv1alpha1.MaaSSubscription{
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					Phase:             maasv1alpha1.PhaseActive,
					RateLimitIdentity: RateLimitIdentityShort,
				},
			},
			want: RateLimitIdentityShort,
		},
		{
			name: "sticky status legacy on empty phase",
			sub: &maasv1alpha1.MaaSSubscription{
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					RateLimitIdentity: RateLimitIdentityLegacy,
				},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "spec short overrides sticky legacy status",
			sub: &maasv1alpha1.MaaSSubscription{
				Spec: maasv1alpha1.MaaSSubscriptionSpec{RateLimitIdentity: RateLimitIdentityShort},
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					Phase:             maasv1alpha1.PhaseActive,
					RateLimitIdentity: RateLimitIdentityLegacy,
				},
			},
			want: RateLimitIdentityShort,
		},
		{
			name: "spec legacy overrides sticky short status",
			sub: &maasv1alpha1.MaaSSubscription{
				Spec: maasv1alpha1.MaaSSubscriptionSpec{RateLimitIdentity: RateLimitIdentityLegacy},
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					RateLimitIdentity: RateLimitIdentityShort,
				},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "legacy annotation migrates when status unset",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationRateLimitIdentity: RateLimitIdentityLegacy},
				},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "status wins over stale annotation",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{AnnotationRateLimitIdentity: RateLimitIdentityLegacy},
				},
				Status: maasv1alpha1.MaaSSubscriptionStatus{
					RateLimitIdentity: RateLimitIdentityShort,
				},
			},
			want: RateLimitIdentityShort,
		},
		{
			// Status can land before the sticky identity on a brand-new CR.
			// Creation after process start must stay short so TRLP does not
			// grandfather new subscriptions onto selected_subscription_key.
			name: "post-start CR with status but no identity is short",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(rateLimitIdentityEpoch.Add(time.Minute)),
				},
				Status: maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
			},
			want: RateLimitIdentityShort,
		},
		{
			name: "pre-start CR with status but no identity is legacy",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(rateLimitIdentityEpoch.Add(-time.Hour)),
				},
				Status: maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "persisted cleanup finalizer is legacy even with empty status",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers: []string{maasSubscriptionFinalizer},
				},
			},
			want: RateLimitIdentityLegacy,
		},
		{
			name: "finalizer wins over post-start creation timestamp",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: metav1.NewTime(rateLimitIdentityEpoch.Add(time.Minute)),
					Finalizers:        []string{maasSubscriptionFinalizer},
				},
				Status: maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
			},
			want: RateLimitIdentityLegacy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, classifiedRateLimitIdentity(tc.sub))
		})
	}
}

func trlpRateLimitPredicate(subNS, subName, modelNS, modelName string) string {
	id := SubscriptionRateLimitID(ModelScopedSubscriptionKey(subNS, subName, modelNS, modelName))
	return `auth.identity.selected_subscription_id == "` + id + `" && !request.path.endsWith("/v1/models")`
}

func trlpRateLimitKeyPredicate(subNS, subName, modelNS, modelName string) string {
	key := ModelScopedSubscriptionKey(subNS, subName, modelNS, modelName)
	return `auth.identity.selected_subscription_key == "` + key + `" && !request.path.endsWith("/v1/models")`
}
