package api_keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

const (
	// KeyPrefix is the prefix for all OpenShift AI API keys
	// Per Feature Refinement: "Simple Opaque Key Format" - keys must be short, opaque strings
	// with a recognizable prefix matching industry standards (OpenAI, Stripe, GitHub).
	KeyPrefix = "sk-oai-"

	// entropyBytes is the number of random bytes to generate (256 bits).
	entropyBytes = 32

	// displayPrefixLength is the number of chars to show in the display prefix (after sk-oai-).
	displayPrefixLength = 12
)

// hashSalt is the constant salt used for API key hashing.
// This is set at startup via SetHashSalt() from configuration.
var hashSalt string

// SetHashSalt sets the constant salt used for API key hashing.
// Must be called before any hash operations. Salt must be at least 32 characters.
func SetHashSalt(salt string) error {
	if len(salt) < 32 {
		return fmt.Errorf("hash salt must be at least 32 characters, got %d", len(salt))
	}
	hashSalt = salt
	return nil
}

// GenerateAPIKey creates a new API key with format: sk-oai-{base62_encoded_256bit_random}
// Returns: (plaintext_key, sha256_hash, display_prefix, error)
//
// Security properties (per Feature Refinement "Key Format & Security"):
// - 256 bits of cryptographic entropy
// - Base62 encoding (alphanumeric only, URL-safe)
// - SHA-256 hash for storage (plaintext never stored)
// - Display prefix for UI identification.
//
//nolint:nonamedreturns // Named returns improve readability for multiple return values.
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	// 1. Generate 32 bytes (256 bits) of cryptographic entropy
	entropy := make([]byte, entropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return "", "", "", fmt.Errorf("failed to generate entropy: %w", err)
	}

	// 2. Encode to base62 (alphanumeric only, no special characters)
	encoded := encodeBase62(entropy)

	// 3. Construct key with OpenShift AI prefix
	plaintext = KeyPrefix + encoded

	// 4. Compute salted SHA-256 hash for storage (uses constant salt from config)
	hash = HashAPIKey(plaintext)

	// 5. Create display prefix (first 12 chars + ellipsis)
	if len(encoded) >= displayPrefixLength {
		prefix = KeyPrefix + encoded[:displayPrefixLength] + "..."
	} else {
		prefix = KeyPrefix + encoded + "..."
	}

	return plaintext, hash, prefix, nil
}

// HashAPIKey computes salted SHA-256 hash of an API key using the constant salt.
// Returns the hash as a 64-character hex string.
// The constant salt must be set via SetHashSalt() before calling this function.
func HashAPIKey(key string) string {
	// Compute salted hash: SHA-256(salt + key)
	h := sha256.New()
	h.Write([]byte(hashSalt))
	h.Write([]byte(key))
	hash := h.Sum(nil)
	return hex.EncodeToString(hash)
}

// ValidateAPIKeyHash validates an API key against its stored hash.
// Uses constant salt from configuration for O(1) indexed lookup.
func ValidateAPIKeyHash(key, storedHash string) bool {
	// Validate hash length (32 bytes = 64 hex chars)
	if len(storedHash) != 64 {
		return false
	}

	// Decode expected hash from hex
	expectedHash, err := hex.DecodeString(storedHash)
	if err != nil {
		return false
	}

	// Compute hash using constant salt
	h := sha256.New()
	h.Write([]byte(hashSalt))
	h.Write([]byte(key))
	computedHash := h.Sum(nil)

	// Compare hashes using constant-time comparison to prevent timing attacks
	return subtle.ConstantTimeCompare(expectedHash, computedHash) == 1
}

// IsValidKeyFormat checks if a key has the correct sk-oai-* prefix and valid base62 body.
func IsValidKeyFormat(key string) bool {
	if !strings.HasPrefix(key, KeyPrefix) {
		return false
	}

	body := key[len(KeyPrefix):]
	if len(body) == 0 {
		return false // Reject empty body
	}

	// Validate base62 charset (0-9, A-Z, a-z)
	for _, c := range body {
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
