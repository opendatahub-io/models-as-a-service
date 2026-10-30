package subscription_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestSelectorCanonicalizeSubscription(t *testing.T) {
	target := createSubscription("subscription-a", []string{"users"}, nil, 10, 1000, "", "")
	metadata, ok := target.Object["metadata"].(map[string]any)
	require.True(t, ok)
	metadata["namespace"] = "test-ns"

	otherNamespace := createSubscription("subscription-a", []string{"users"}, nil, 10, 1000, "", "")
	otherMetadata, ok := otherNamespace.Object["metadata"].(map[string]any)
	require.True(t, ok)
	otherMetadata["namespace"] = "other-ns"

	tests := []struct {
		name          string
		requested     string
		subscriptions []*unstructured.Unstructured
		want          string
		wantErr       string
	}{
		{
			name:          "qualified alias resolves to stored name",
			requested:     "test-ns/subscription-a",
			subscriptions: []*unstructured.Unstructured{target, otherNamespace},
			want:          "subscription-a",
		},
		{
			name:          "plain name remains canonical",
			requested:     "subscription-a",
			subscriptions: []*unstructured.Unstructured{target},
			want:          "subscription-a",
		},
		{
			name:          "unknown subscription fails closed",
			requested:     "test-ns/unknown",
			subscriptions: []*unstructured.Unstructured{target},
			wantErr:       "not_found",
		},
		{
			name:          "ambiguous plain name fails closed",
			requested:     "subscription-a",
			subscriptions: []*unstructured.Unstructured{target, otherNamespace},
			wantErr:       "ambiguous_subscription",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := subscription.NewSelector(
				logger.New(false),
				&fakeLister{subscriptions: test.subscriptions},
				nil,
				nil,
			)

			got, err := selector.CanonicalizeSubscription(test.requested)

			var adminErr *subscription.SubscriptionAdminError
			if test.wantErr != "" {
				require.Error(t, err)
				require.ErrorAs(t, err, &adminErr)
				assert.Equal(t, test.wantErr, adminErr.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
