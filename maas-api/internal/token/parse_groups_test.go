package token_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestParseGroupsHeader_JSONArray(t *testing.T) {
	groups, err := token.ParseGroupsHeader(`["ai-eng", "platform"]`)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai-eng", "platform"}, groups)
}

func TestParseGroupsHeader_SingleJSON(t *testing.T) {
	groups, err := token.ParseGroupsHeader(`["ai-eng"]`)
	require.NoError(t, err)
	assert.Equal(t, []string{"ai-eng"}, groups)
}

func TestParseGroupsHeader_AuthorinoFormat(t *testing.T) {
	groups, err := token.ParseGroupsHeader("[ai-eng]")
	require.NoError(t, err)
	assert.Equal(t, []string{"ai-eng"}, groups)
}

func TestParseGroupsHeader_AuthorinoMultiple(t *testing.T) {
	groups, err := token.ParseGroupsHeader("[ai-eng platform ops]")
	require.NoError(t, err)
	assert.Equal(t, []string{"ai-eng", "platform", "ops"}, groups)
}

func TestParseGroupsHeader_Empty(t *testing.T) {
	_, err := token.ParseGroupsHeader("")
	assert.Error(t, err)
}

func TestParseGroupsHeader_EmptyBrackets(t *testing.T) {
	groups, err := token.ParseGroupsHeader("[]")
	require.NoError(t, err)
	assert.Equal(t, []string{}, groups)
}

func TestParseGroupsHeader_EmptyAuthorinoBrackets(t *testing.T) {
	_, err := token.ParseGroupsHeader("[ ]")
	assert.Error(t, err)
}

func TestParseGroupsHeader_PreservesWhitespaceAndDropsEmptyEntries(t *testing.T) {
	groups, err := token.ParseGroupsHeader(`["  ", ""]`)
	require.NoError(t, err)
	assert.Equal(t, []string{"  "}, groups)
}

func TestParseGroupsHeaderPreservesOpaqueValues(t *testing.T) {
	input := []string{" users ", `quote"slash\`, "é漢字", "zero\x00byte", "comma,pipe|"} //nolint:gosmopolitan // Testing Unicode handling
	header, err := json.Marshal(input)
	require.NoError(t, err)

	groups, err := token.ParseGroupsHeader(string(header))
	require.NoError(t, err)
	expected := input
	assert.Equal(t, expected, groups)
}

func TestParseGroupsHeaderTrimsLegacyTokens(t *testing.T) {
	groups, err := token.ParseGroupsHeader("[ team-a  team-b ]")
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a", "team-b"}, groups)
}
