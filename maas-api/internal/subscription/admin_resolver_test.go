package subscription_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const (
	adminTestOrganizationA = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	adminTestOrganizationB = "5a9d6b0e-9d0f-4c9f-ae07-916e3576d9fa"
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

func TestResolveSubscriptionAdminUsesFlatOrganizationRole(t *testing.T) {
	organizationASubscription := createSubscriptionForAdmin(
		t, "subscription-a", "test-ns", adminTestOrganizationA, "", "Active", nil,
	)
	organizationBSubscription := createSubscriptionForAdmin(
		t, "subscription-b", "test-ns", adminTestOrganizationB, "", "Active", nil,
	)
	selector := subscription.NewSelector(
		logger.New(false),
		&mockLister{subscriptions: []*unstructured.Unstructured{
			organizationASubscription,
			organizationBSubscription,
		}},
		nil,
		nil,
		subscription.WithTenantName("test-tenant"),
	)
	identity := flatAdminIdentity(t, adminTestOrganizationA, []string{"admins"})

	scope, err := selector.ResolveSubscriptionAdmin(identity, "test-ns/subscription-a")
	require.NoError(t, err)
	assert.Equal(t, adminTestOrganizationA, scope.OrganizationID)

	_, err = selector.ResolveSubscriptionAdmin(identity, "test-ns/subscription-b")
	var adminErr *subscription.SubscriptionAdminError
	require.ErrorAs(t, err, &adminErr)
	assert.Equal(t, "access_denied", adminErr.Code)
}

func TestResolveSubscriptionAdminScopesUsesFlatOrganizationRole(t *testing.T) {
	organizationASubscription := createSubscriptionForAdmin(
		t, "subscription-a", "test-ns", adminTestOrganizationA, "", "Active", nil,
	)
	organizationBSubscription := createSubscriptionForAdmin(
		t, "subscription-b", "test-ns", adminTestOrganizationB, "", "Active", nil,
	)
	selector := subscription.NewSelector(
		logger.New(false),
		&mockLister{subscriptions: []*unstructured.Unstructured{
			organizationASubscription,
			organizationBSubscription,
		}},
		nil,
		nil,
		subscription.WithTenantName("test-tenant"),
	)
	identity := flatAdminIdentity(t, adminTestOrganizationA, []string{"admins"})

	scopes, err := selector.ResolveSubscriptionAdminScopes(identity)
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, "test-tenant", scopes[0].Tenant)
	assert.Equal(t, "test-ns", scopes[0].Namespace)
	assert.Equal(t, "subscription-a", scopes[0].Subscription)
	assert.Equal(t, adminTestOrganizationA, scopes[0].OrganizationID)

	noAdminIdentity := flatAdminIdentity(t, adminTestOrganizationA, []string{"users"})
	noAdminSelector := subscription.NewSelector(
		logger.New(false),
		&mockLister{},
		nil,
		nil,
		subscription.WithTenantName("test-tenant"),
	)

	scopes, err = noAdminSelector.ResolveSubscriptionAdminScopes(noAdminIdentity)
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

func TestResolveSubscriptionAdminRequiresAdminsRoleAndOIDCSource(t *testing.T) {
	subscriptionObject := createSubscriptionForAdmin(
		t, "subscription-a", "test-ns", adminTestOrganizationA, "", "Active", nil,
	)
	selector := subscription.NewSelector(
		logger.New(false),
		&mockLister{subscriptions: []*unstructured.Unstructured{subscriptionObject}},
		nil,
		nil,
		subscription.WithTenantName("test-tenant"),
	)

	tests := []struct {
		name   string
		roles  []string
		source string
	}{
		{name: "empty roles", roles: []string{}},
		{name: "users role", roles: []string{"users"}},
		{name: "unknown role", roles: []string{"owner"}},
		{name: "API key identity", roles: []string{"admins"}, source: "api-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := flatAdminIdentity(t, adminTestOrganizationA, test.roles)
			if test.source != "" {
				identity.Source = test.source
			}

			_, err := selector.ResolveSubscriptionAdmin(identity, "test-ns/subscription-a")
			var adminErr *subscription.SubscriptionAdminError
			require.ErrorAs(t, err, &adminErr)
			assert.Equal(t, "access_denied", adminErr.Code)
		})
	}
}

func flatAdminIdentity(t *testing.T, organizationID string, roles []string) *token.VerifiedIdentity {
	t.Helper()
	envelope, err := json.Marshal(map[string]any{
		"source": "oidc",
		"claims": map[string]any{
			"iss":    "https://issuer.example.com",
			"sub":    "admin-user",
			"aud":    "maas-api",
			"azp":    "maas-client",
			"org_id": organizationID,
			"roles":  roles,
		},
	})
	require.NoError(t, err)
	identity, err := token.ParseIdentityEnvelope(string(envelope))
	require.NoError(t, err)
	return identity
}
