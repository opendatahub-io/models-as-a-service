package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// KeycloakConfig holds Keycloak configuration
type KeycloakConfig struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
	Audience     string
}

// KeycloakTokenManager handles token minting via Keycloak token exchange
type KeycloakTokenManager struct {
	config KeycloakConfig
	client *http.Client
	logger *logger.Logger
}

// NewKeycloakTokenManager creates a new Keycloak token manager
func NewKeycloakTokenManager(config KeycloakConfig, log *logger.Logger) *KeycloakTokenManager {
	if log == nil {
		log = logger.Production()
	}
	return &KeycloakTokenManager{
		config: config,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: log,
	}
}

// TokenExchangeRequest represents a Keycloak token exchange request
type TokenExchangeRequest struct {
	SubjectToken       string `json:"subject_token"`
	SubjectTokenType   string `json:"subject_token_type"`
	RequestedTokenType string `json:"requested_token_type"`
	Audience           string `json:"audience,omitempty"`
	RequestedSubject   string `json:"requested_subject,omitempty"`
}

// TokenExchangeResponse represents a Keycloak token exchange response
type TokenExchangeResponse struct {
	AccessToken     string `json:"access_token"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int    `json:"expires_in"`
	RefreshToken    string `json:"refresh_token,omitempty"`
	IssuedTokenType string `json:"issued_token_type"`
}

// GenerateToken creates a Keycloak token by using the user's existing token directly
// The user's token already contains preferred_username and groups, so no exchange is needed
func (k *KeycloakTokenManager) GenerateToken(ctx context.Context, user *UserContext, userKeycloakToken string, expiration time.Duration) (*Token, error) {
	log := k.logger.WithFields(
		"username", user.Username,
		"expiration", expiration.String(),
	)

	// Use the user's token directly - it already contains all the user info we need
	// (preferred_username, groups, etc.) so no token exchange is necessary
	keycloakToken := userKeycloakToken
	expiresIn := int(expiration.Seconds())

	// Parse the Keycloak token to extract claims
	claims, err := extractClaims(keycloakToken)
	if err != nil {
		return nil, fmt.Errorf("failed to extract claims from Keycloak token: %w", err)
	}

	// Extract JTI
	jti, _ := claims["jti"].(string)
	if jti == "" {
		// Fallback: use jti from token if available, otherwise generate
		jti, _ = claims["jti"].(string)
		if jti == "" {
			var errJTI error
			jti, errJTI = generateLocalJTI()
			if errJTI != nil {
				return nil, fmt.Errorf("jti claim not found and fallback generation failed: %w", errJTI)
			}
		}
	}

	// Extract issued at
	var issuedAt int64
	if iatClaim, ok := claims["iat"].(float64); ok {
		issuedAt = int64(iatClaim)
	} else {
		// Fallback to current time if iat is missing
		issuedAt = time.Now().Unix()
	}

	// Calculate expiration timestamp
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()

	log.Debug("Successfully generated Keycloak token",
		"expires_at", expiresAt,
		"jti", jti,
	)

	return &Token{
		Token:      keycloakToken,
		Expiration: Duration{expiration},
		ExpiresAt:  expiresAt,
		IssuedAt:   issuedAt,
		JTI:        jti,
	}, nil
}

// exchangeUserToken exchanges the user's Keycloak token for a new token with the correct audience
// This preserves the user's identity (preferred_username, groups) in the exchanged token
func (k *KeycloakTokenManager) exchangeUserToken(ctx context.Context, userToken string, expiration time.Duration) (string, int, error) {
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", k.config.BaseURL, k.config.Realm)

	data := url.Values{}
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("subject_token", userToken)
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("client_id", k.config.ClientID)
	data.Set("client_secret", k.config.ClientSecret)
	if k.config.Audience != "" {
		data.Set("audience", k.config.Audience)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := k.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %w", err)
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = int(expiration.Seconds())
	}

	return tokenResp.AccessToken, expiresIn, nil
}

// getClientCredentialsToken gets a token from Keycloak using client credentials
// This is kept for backward compatibility but should not be used for user tokens
func (k *KeycloakTokenManager) getClientCredentialsToken(ctx context.Context, expiration time.Duration) (string, int, error) {
	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", k.config.BaseURL, k.config.Realm)

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", k.config.ClientID)
	data.Set("client_secret", k.config.ClientSecret)
	if k.config.Audience != "" {
		data.Set("audience", k.config.Audience)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := k.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %w", err)
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = int(expiration.Seconds())
	}

	return tokenResp.AccessToken, expiresIn, nil
}

// RevokeTokens revokes tokens by invalidating them in Keycloak
// For PoC, this is a placeholder - in production you'd use Keycloak's revocation endpoint
func (k *KeycloakTokenManager) RevokeTokens(ctx context.Context, user *UserContext) error {
	// Keycloak doesn't support per-user token revocation easily
	// In production, you'd need to:
	// 1. Track issued tokens in a database
	// 2. Use Keycloak's logout endpoint or token revocation
	// 3. Or use short-lived tokens and rely on expiration
	k.logger.Debug("Token revocation requested",
		"username", user.Username,
		"note", "Keycloak token revocation requires token tracking or short TTL",
	)
	return nil
}
