package subscription_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
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

func TestSelectorResolveSubscriptionAdminScopes(t *testing.T) {
	adminSubscription := createSubscription("subscription-a", nil, nil, 10, 1000, "", "")
	require.NoError(t, unstructured.SetNestedField(
		adminSubscription.Object,
		"42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
		"spec", "organization", "id",
	))
	otherSubscription := createSubscription("subscription-b", nil, nil, 10, 1000, "", "")
	require.NoError(t, unstructured.SetNestedField(
		otherSubscription.Object,
		"11111111-2222-3333-4444-555555555555",
		"spec", "organization", "id",
	))

	selector := subscription.NewSelector(
		logger.New(false),
		&fakeLister{subscriptions: []*unstructured.Unstructured{
			adminSubscription,
			otherSubscription,
		}},
		nil,
		nil,
		subscription.WithTenantName("grid-tenant"),
	)
	identity := &token.VerifiedIdentity{
		Source: "oidc",
		Claims: token.VerifiedClaims{
			Organizations: []token.OrganizationMembership{{
				OrganizationID: "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
				Roles:          []string{"admins"},
			}},
		},
	}

	scopes, err := selector.ResolveSubscriptionAdminScopes(identity)

	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, "grid-tenant", scopes[0].Tenant)
	assert.Equal(t, "test-ns", scopes[0].Namespace)
	assert.Equal(t, "subscription-a", scopes[0].Subscription)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", scopes[0].OrganizationID)

	noAdminSelector := subscription.NewSelector(
		logger.New(false),
		&fakeLister{err: errors.New("subscription inventory must not be loaded")},
		nil,
		nil,
		subscription.WithTenantName("grid-tenant"),
	)
	noAdminIdentity := &token.VerifiedIdentity{
		Source: "oidc",
		Claims: token.VerifiedClaims{
			Organizations: []token.OrganizationMembership{{
				OrganizationID: "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
				Roles:          []string{"users"},
			}},
		},
	}

	scopes, err = noAdminSelector.ResolveSubscriptionAdminScopes(noAdminIdentity)

	require.NoError(t, err)
	assert.Empty(t, scopes)
}
