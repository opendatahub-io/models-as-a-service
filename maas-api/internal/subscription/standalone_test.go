package subscription_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestDisabledSelectorFailsClosed(t *testing.T) {
	selector := subscription.DisabledSelector{}
	assert.False(t, selector.Enabled())

	_, err := selector.Select(nil, "alice", "some-subscription", "")
	require.ErrorIs(t, err, subscription.ErrSubscriptionsDisabled)

	_, err = selector.SelectHighestPriority(nil, "alice")
	require.ErrorIs(t, err, subscription.ErrSubscriptionsDisabled)

	_, err = selector.GetAllAccessible(nil, "alice")
	require.ErrorIs(t, err, subscription.ErrSubscriptionsDisabled)

	_, err = selector.ResolveSubscriptionAdmin(nil, "some-subscription")
	require.Error(t, err)
	var adminErr *subscription.SubscriptionAdminError
	require.ErrorAs(t, err, &adminErr)
	assert.Equal(t, "subscriptions_disabled", adminErr.Code)

	_, err = selector.CanonicalizeSubscription("some-subscription")
	require.Error(t, err)
	require.ErrorAs(t, err, &adminErr)
	assert.Equal(t, "subscriptions_disabled", adminErr.Code)
}
