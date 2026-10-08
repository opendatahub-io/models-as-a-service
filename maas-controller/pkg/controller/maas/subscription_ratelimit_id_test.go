package maas

import (
	"testing"

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

	shortAnn := map[string]string{AnnotationRateLimitIdentity: RateLimitIdentityShort}
	legacyAnn := map[string]string{AnnotationRateLimitIdentity: RateLimitIdentityLegacy}

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
			name: "explicit short wins over status",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{Annotations: shortAnn},
				Status:     maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
			},
			want: RateLimitIdentityShort,
		},
		{
			name: "explicit legacy wins on empty status",
			sub: &maasv1alpha1.MaaSSubscription{
				ObjectMeta: metav1.ObjectMeta{Annotations: legacyAnn},
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

func TestStampRateLimitIdentityAnnotation_Sticky(t *testing.T) {
	t.Parallel()

	fresh := &maasv1alpha1.MaaSSubscription{}
	assert.True(t, stampRateLimitIdentityAnnotation(fresh))
	assert.Equal(t, RateLimitIdentityShort, fresh.Annotations[AnnotationRateLimitIdentity])
	assert.False(t, stampRateLimitIdentityAnnotation(fresh), "must not rewrite a set annotation")

	legacy := &maasv1alpha1.MaaSSubscription{
		Status: maasv1alpha1.MaaSSubscriptionStatus{Phase: maasv1alpha1.PhaseActive},
	}
	assert.True(t, stampRateLimitIdentityAnnotation(legacy))
	assert.Equal(t, RateLimitIdentityLegacy, legacy.Annotations[AnnotationRateLimitIdentity])

	// Later status must not flip a short annotation to legacy.
	fresh.Status.Phase = maasv1alpha1.PhaseActive
	assert.False(t, stampRateLimitIdentityAnnotation(fresh))
	assert.Equal(t, RateLimitIdentityShort, fresh.Annotations[AnnotationRateLimitIdentity])
}

func trlpRateLimitPredicate(subNS, subName, modelNS, modelName string) string {
	id := SubscriptionRateLimitID(ModelScopedSubscriptionKey(subNS, subName, modelNS, modelName))
	return `auth.identity.selected_subscription_id == "` + id + `" && !request.path.endsWith("/v1/models")`
}

func trlpRateLimitKeyPredicate(subNS, subName, modelNS, modelName string) string {
	key := ModelScopedSubscriptionKey(subNS, subName, modelNS, modelName)
	return `auth.identity.selected_subscription_key == "` + key + `" && !request.path.endsWith("/v1/models")`
}
