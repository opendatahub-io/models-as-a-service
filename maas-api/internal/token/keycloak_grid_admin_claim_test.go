package token_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type keycloakRealmFixture struct {
	Realm   string           `json:"realm"`
	Clients []keycloakClient `json:"clients"`
	Users   []keycloakUser   `json:"users"`
}

type keycloakClient struct {
	ClientID        string                   `json:"clientId"`
	ProtocolMappers []keycloakProtocolMapper `json:"protocolMappers"`
}

type keycloakProtocolMapper struct {
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

type keycloakUser struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
}

func TestKeycloakGridAdminRealmFixtureContainsOrganizationClaimMapper(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/grid-admin-realm.json")
	require.NoError(t, err)

	var realm keycloakRealmFixture
	require.NoError(t, json.Unmarshal(data, &realm))

	require.Equal(t, "grid-admin", realm.Realm)
	require.NotEmpty(t, realm.Clients)

	client := realm.Clients[0]
	require.Equal(t, "test-client", client.ClientID)

	var mapper *keycloakProtocolMapper
	for index := range client.ProtocolMappers {
		if client.ProtocolMappers[index].Name == "maas-organizations" {
			mapper = &client.ProtocolMappers[index]
			break
		}
	}
	require.NotNil(t, mapper, "expected maas-organizations protocol mapper")
	assert.Equal(t, "openid-connect", mapper.Protocol)
	assert.Equal(t, "https://redhat.com/maas/orgs", mapper.Config["claim.name"])
	assert.Equal(t, "true", mapper.Config["access.token.claim"])

	require.Len(t, realm.Users, 4)
	assert.Equal(t, "org-a-admin", realm.Users[0].Username)
	assert.Equal(t, "org-b-user", realm.Users[1].Username)
	assert.Equal(t, "dual-member", realm.Users[2].Username)
	assert.Equal(t, "no-org", realm.Users[3].Username)
}
