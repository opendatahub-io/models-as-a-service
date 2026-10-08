package subscription_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestRateLimitID_KnownVector(t *testing.T) {
	key := "models-as-a-service/demo-sub@llm/sim-chat"
	assert.Equal(t, "15d4c0904b3ebaf9", subscription.RateLimitID(key))
	assert.Equal(t, "15d4c0904b3ebaf9", subscription.RateLimitIDFor("models-as-a-service", "demo-sub", "llm/sim-chat"))
	assert.Empty(t, subscription.RateLimitIDFor("models-as-a-service", "demo-sub", ""))
}
