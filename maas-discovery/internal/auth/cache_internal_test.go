package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwtClaim builds an unsigned three-part token whose payload is the given
// JSON claims map.
func jwtClaim(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
}

// TestTokenExpiry_JWTParsing covers the JWT exp extraction used to cap cache
// TTLs. The claim is read without verification; only the delegate's
// TokenReview authenticates the token. RFC 7519 NumericDates may be
// non-integer, so fractional and exponent forms must parse; a present but
// unparseable exp is an error so the caller refuses to cache the decision.
func TestTokenExpiry_JWTParsing(t *testing.T) {
	exp := time.Unix(1798725200, 0).UTC()

	got, ok, err := tokenExpiry(jwtClaim(t, map[string]any{"exp": exp.Unix()}))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, exp, got)

	// Fractional NumericDate floors to the second (never rounds up).
	got, ok, err = tokenExpiry(jwtClaim(t, map[string]any{"exp": float64(exp.Unix()) + 0.9}))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, exp, got, "fractional exp must floor, not extend")

	// Exponent form.
	got, ok, err = tokenExpiry(jwtClaim(t, map[string]any{"exp": 2e9}))
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, time.Unix(2000000000, 0).UTC(), got)

	// Non-JWT and no-claim tokens report no expiry (configured TTL applies).
	_, ok, err = tokenExpiry("not-a-jwt")
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = tokenExpiry("only.two")
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = tokenExpiry("e30.e30.c2ln")
	require.NoError(t, err)
	assert.False(t, ok, "JWT without an exp claim must not report an expiry")

	// Present but unparseable exp: an error, so the decision is not cached.
	_, _, err = tokenExpiry(jwtClaim(t, map[string]any{"exp": "not-a-number"}))
	require.Error(t, err, "string exp must be refused")

	_, _, err = tokenExpiry("e30." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"exp":null}`)) + ".c2ln")
	require.Error(t, err, "null exp must be refused")
}
