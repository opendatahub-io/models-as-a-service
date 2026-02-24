package api_keys

import (
	"crypto/rand"
	"crypto/sha256"
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

	// 4. Compute SHA-256 hash for storage
	hash = HashAPIKey(plaintext)

	// 5. Create display prefix (first 12 chars + ellipsis)
	if len(encoded) >= displayPrefixLength {
		prefix = KeyPrefix + encoded[:displayPrefixLength] + "..."
	} else {
		prefix = KeyPrefix + encoded + "..."
	}

	return plaintext, hash, prefix, nil
}

// HashAPIKey computes SHA-256 hash of an API key (for validation and storage)
// This is the canonical hashing function - used by both key creation and validation.
func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// IsValidKeyFormat checks if a key has the correct sk-oai-* prefix.
func IsValidKeyFormat(key string) bool {
	return strings.HasPrefix(key, KeyPrefix)
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
