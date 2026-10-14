package subscription_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestSubscriptionInfoSerializesGridAdminContract(t *testing.T) {
	t.Parallel()

	info := subscription.SubscriptionInfo{
		SubscriptionIDHeader:    "premium",
		SubscriptionDescription: "Premium Plan",
		Priority:                10,
		ModelRefs:               []subscription.ModelRefInfo{},
		Roles:                   []string{"owner", "admin"},
		Owner: &subscription.SubscriptionOwner{
			Groups: []string{"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/ai-grid-users"},
			Users:  []string{},
		},
	}

	encoded, err := json.Marshal(info)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &raw))

	require.Contains(t, raw, "subscription_id_header")
	require.Contains(t, raw, "subscription_description")
	require.Contains(t, raw, "priority")
	require.Contains(t, raw, "model_refs")
	require.Contains(t, raw, "roles")
	require.Contains(t, raw, "owner")

	var roles []string
	require.NoError(t, json.Unmarshal(raw["roles"], &roles))
	assert.Equal(t, []string{"owner", "admin"}, roles)

	var owner struct {
		Groups []string `json:"groups"`
		Users  []string `json:"users"`
	}
	require.NoError(t, json.Unmarshal(raw["owner"], &owner))
	assert.Equal(t, []string{"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/ai-grid-users"}, owner.Groups)
	assert.Empty(t, owner.Users)
}
