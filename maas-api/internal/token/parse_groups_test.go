package token_test

import (
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
	_, err := token.ParseGroupsHeader("[]")
	assert.Error(t, err)
}

func TestParseGroupsHeader_EmptyAuthorinoBrackets(t *testing.T) {
	_, err := token.ParseGroupsHeader("[ ]")
	assert.Error(t, err)
}

func TestParseGroupsHeader_WhitespaceOnlyJSON(t *testing.T) {
	_, err := token.ParseGroupsHeader(`["  ", ""]`)
	assert.Error(t, err)
}
