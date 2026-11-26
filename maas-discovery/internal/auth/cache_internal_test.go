package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenExpiry_JWTParsing covers the JWT exp extraction used to cap cache
// TTLs. The claim is read without verification; only the delegate's
// TokenReview authenticates the token.
func TestTokenExpiry_JWTParsing(t *testing.T) {
	exp := time.Date(2026, 11, 26, 13, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	require.NoError(t, err)

	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	got, ok := tokenExpiry(token)
	require.True(t, ok)
	assert.Equal(t, exp, got)

	_, ok = tokenExpiry("not-a-jwt")
	assert.False(t, ok)

	_, ok = tokenExpiry("only.two")
	assert.False(t, ok)

	// payload is valid base64 but not JSON
	_, ok = tokenExpiry("e30.Zm9v.c2ln")
	assert.False(t, ok)

	// JWT without an exp claim
	_, ok = tokenExpiry("e30.e30.c2ln")
	assert.False(t, ok)
}
