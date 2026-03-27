package api_keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	// KeyPrefix is the prefix for all OpenShift AI API keys.
	// Per Feature Refinement: "Simple Opaque Key Format" - keys must be short, opaque strings
	// with a recognizable prefix matching industry standards (OpenAI, Stripe, GitHub).
	KeyPrefix = "sk-oai-"

	// KeyIDSeparator separates the key_id from the secret in the API key.
	KeyIDSeparator = "_"

	// keyIDBytes is the number of random bytes for key_id (96 bits → ~16 base62 chars).
	keyIDBytes = 12

	// entropyBytes is the number of random bytes for the secret (256 bits).
	entropyBytes = 32

	// displayPrefixLength is the number of chars to show in the display prefix.
	displayPrefixLength = 12
)

// GenerateAPIKey creates a new API key with format: sk-oai-{key_id}_{secret}
// Returns: (plaintext_key, sha256_hash, display_prefix, error)
//
// Security properties (per Feature Refinement "Key Format & Security"):
// - key_id: 96-bit random identifier (~16 base62 chars, guaranteed >= 12), used as unique salt
// - secret: 256 bits of cryptographic entropy (~43 base62 chars)
// - Hash: SHA-256(key_id + "\x00" + secret) - null delimiter prevents length-ambiguity attacks
// - Base62 encoding (alphanumeric only, URL-safe)
// - Display prefix shows first 12 chars of key_id for UI identification.
// - Use ParseAPIKey() to extract key_id and secret if needed.
//
//nolint:nonamedreturns // Named returns improve readability for multiple return values.
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	// 1. Generate key_id (96 bits → ~16 base62 chars)
	keyIDEntropy := make([]byte, keyIDBytes)
	if _, err := rand.Read(keyIDEntropy); err != nil {
		return "", "", "", fmt.Errorf("failed to generate key_id entropy: %w", err)
	}
	keyID := encodeBase62(keyIDEntropy)

	// 2. Generate secret (256 bits → ~43 base62 chars)
	secretEntropy := make([]byte, entropyBytes)
	if _, err := rand.Read(secretEntropy); err != nil {
		return "", "", "", fmt.Errorf("failed to generate secret entropy: %w", err)
	}
	secret := encodeBase62(secretEntropy)

	// 3. Construct key: sk-oai-{key_id}_{secret}
	plaintext = KeyPrefix + keyID + KeyIDSeparator + secret

	// 4. Compute salted hash: SHA-256(key_id + secret)
	// key_id serves as a unique per-key salt (FIPS 180-4 compliant)
	hash = hashWithSalt(keyID, secret)

	// 5. Create display prefix (first 12 chars of key_id + ellipsis)
	if len(keyID) >= displayPrefixLength {
		prefix = KeyPrefix + keyID[:displayPrefixLength] + "..."
	} else {
		prefix = KeyPrefix + keyID + "..."
	}

	return plaintext, hash, prefix, nil
}

// hashWithSalt computes SHA-256(keyID + "\x00" + secret) for storage.
// The keyID serves as a unique per-key salt, providing FIPS 180-4 compliant hashing.
// The null byte delimiter prevents length-ambiguity attacks where different keyID/secret
// splits could produce the same hash (e.g., "ab"+"c" vs "a"+"bc").
func hashWithSalt(keyID, secret string) string {
	h := sha256.Sum256([]byte(keyID + "\x00" + secret))
	return hex.EncodeToString(h[:])
}

// ParseAPIKey extracts the key_id and secret from an API key.
// Returns: (key_id, secret, error).
// Key format: sk-oai-{key_id}_{secret}.
func ParseAPIKey(key string) (string, string, error) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return "", "", errors.New("invalid key prefix")
	}

	body := key[len(KeyPrefix):]
	parts := strings.SplitN(body, KeyIDSeparator, 2)
	if len(parts) != 2 {
		return "", "", errors.New("invalid key format: missing separator")
	}

	keyID := parts[0]
	secret := parts[1]

	if keyID == "" || secret == "" {
		return "", "", errors.New("invalid key format: empty key_id or secret")
	}

	return keyID, secret, nil
}

// ValidateAPIKeyHash validates an API key against a stored hash.
// Computes SHA-256(key_id + secret) and compares with stored hash using constant-time comparison.
func ValidateAPIKeyHash(key, storedHash string) bool {
	keyID, secret, err := ParseAPIKey(key)
	if err != nil {
		return false
	}

	computedHash := hashWithSalt(keyID, secret)
	return subtle.ConstantTimeCompare([]byte(computedHash), []byte(storedHash)) == 1
}

// HashAPIKey computes SHA-256 hash of an API key for validation.
// Parses the key to extract key_id and secret, then computes SHA-256(key_id + secret).
// Returns empty string if key format is invalid.
func HashAPIKey(key string) string {
	keyID, secret, err := ParseAPIKey(key)
	if err != nil {
		return ""
	}
	return hashWithSalt(keyID, secret)
}

// IsValidKeyFormat checks if a key has the correct format: sk-oai-{key_id}_{secret}
// Both key_id and secret must be non-empty base62 strings.
func IsValidKeyFormat(key string) bool {
	if !strings.HasPrefix(key, KeyPrefix) {
		return false
	}

	body := key[len(KeyPrefix):]
	parts := strings.SplitN(body, KeyIDSeparator, 2)
	if len(parts) != 2 {
		return false // Must have exactly one separator
	}

	keyID := parts[0]
	secret := parts[1]

	if keyID == "" || secret == "" {
		return false // Both parts must be non-empty
	}

	// Validate key_id is base62
	if !isBase62(keyID) {
		return false
	}

	// Validate secret is base62
	if !isBase62(secret) {
		return false
	}

	return true
}

// isBase62 checks if a string contains only base62 characters (0-9, A-Z, a-z).
func isBase62(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// encodeBase62 converts byte array to base62 string
// Base62 uses 0-9, A-Z, a-z (no special characters, URL-safe).
func encodeBase62(data []byte) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	n := new(big.Int).SetBytes(data)
	base := big.NewInt(62)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append([]byte{alphabet[mod.Int64()]}, result...)
	}

	// Handle zero input
	if len(result) == 0 {
		return string(alphabet[0])
	}

	return string(result)
}
