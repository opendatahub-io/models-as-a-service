package token_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestParseIdentityEnvelope_PreservesOrganizationScopedRoles(t *testing.T) {
	t.Parallel()

	identity := token.VerifiedIdentity{
		Source: "oidc",
		Claims: token.VerifiedClaims{
			Issuer:          "https://issuer.example.com",
			Subject:         "user-1",
			Audience:        token.Audience{Value: "maas-api"},
			AuthorizedParty: "maas-client",
			Groups: []string{
				"42c3e46f-2477-44d7-a85b-d3b43f6b31fa/ai-grid-users",
				"b93d4dd4-cba0-4d4a-b5f0-bab631f4161f/ai-grid-users",
			},
			Organizations: []token.OrganizationMembership{
				{
					OrganizationID: "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
					Roles:          []string{"admins"},
				},
				{
					OrganizationID: "b93d4dd4-cba0-4d4a-b5f0-bab631f4161f",
					Roles:          []string{"users"},
				},
			},
		},
	}

	encoded, err := json.Marshal(identity)
	require.NoError(t, err)

	parsed, err := token.ParseIdentityEnvelope(string(encoded))
	require.NoError(t, err)

	require.Len(t, parsed.Claims.Organizations, 2)
	assert.Equal(t, "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", parsed.Claims.Organizations[0].OrganizationID)
	assert.Equal(t, []string{"admins"}, parsed.Claims.Organizations[0].Roles)
	assert.Equal(t, "b93d4dd4-cba0-4d4a-b5f0-bab631f4161f", parsed.Claims.Organizations[1].OrganizationID)
	assert.Equal(t, []string{"users"}, parsed.Claims.Organizations[1].Roles)
}
