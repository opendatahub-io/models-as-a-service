package token

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// LooksLikeJWT returns true if the token string has the form of a JWT (exactly three
// dot-separated segments). Opaque tokens (e.g. OpenShift "oc whoami -t") are not JWTs.
func LooksLikeJWT(tokenString string) bool {
	parts := strings.SplitN(tokenString, ".", 4)
	return len(parts) == 3
}

// ExtractClaims parses the JWT and extracts claims without validation.
func ExtractClaims(tokenString string) (jwt.MapClaims, error) {
	token, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid token claims")
	}

	return claims, nil
}
