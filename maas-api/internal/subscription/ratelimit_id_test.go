package subscription

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRateLimitID_KnownVector(t *testing.T) {
	key := "models-as-a-service/demo-sub@llm/sim-chat"
	assert.Equal(t, "15d4c0904b3ebaf9", RateLimitID(key))
	assert.Equal(t, "15d4c0904b3ebaf9", RateLimitIDFor("models-as-a-service", "demo-sub", "llm/sim-chat"))
	assert.Equal(t, "", RateLimitIDFor("models-as-a-service", "demo-sub", ""))
}
