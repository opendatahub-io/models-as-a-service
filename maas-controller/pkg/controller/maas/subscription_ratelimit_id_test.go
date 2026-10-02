package maas

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestSubscriptionSelectClause_IncludesIDAndKeyFallback(t *testing.T) {
	key := "ns/sub@models/llm"
	got := subscriptionSelectClause(key)
	want := `(auth.identity.selected_subscription_id == "` + SubscriptionRateLimitID(key) +
		`" || auth.identity.selected_subscription_key == "` + key + `")`
	assert.Equal(t, want, got)
}

func trlpRateLimitPredicate(subNS, subName, modelNS, modelName string) string {
	key := ModelScopedSubscriptionKey(subNS, subName, modelNS, modelName)
	return subscriptionSelectClause(key) + ` && !request.path.endsWith("/v1/models")`
}
