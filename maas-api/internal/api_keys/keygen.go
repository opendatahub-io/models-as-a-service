package api_keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/pbkdf2"
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

	// PBKDF2 security parameters following OWASP 2024 guidelines.
	HashBytes  = 32     // 256 bits  
	Iterations = 600000 // OWASP recommendation for 2024
	
	// Constant salt for operator deployment (acceptable for machine-generated high-entropy keys).
	// SECURITY: This constant salt is ONLY safe for machine-generated 256-bit entropy keys.
	// Rainbow table attacks are infeasible against 256-bit random keys regardless of salt.
	// WARNING: Do NOT use this function for user-chosen passwords - constant salt enables
	// rainbow tables against low-entropy inputs. Use per-user random salts for passwords.
	ConstantSalt = "openshiftai.pbkdf2.salt.v1" // 26 bytes UTF-8
)

// APIKeyHashData represents a hashed API key with PBKDF2 metadata.
type APIKeyHashData struct {
	Hash       string `json:"hash"`       // Hex-encoded PBKDF2 output
	Iterations int    `json:"iterations"` // Number of PBKDF2 iterations
}

// GenerateAPIKey creates a new API key with format: sk-oai-{base62_encoded_256bit_random}
// Returns: (plaintext_key, pbkdf2_hash_data, display_prefix, error)
//
// Security properties (per Feature Refinement "Key Format & Security"):
// - 256 bits of cryptographic entropy
// - Base62 encoding (alphanumeric only, URL-safe)
// - PBKDF2-HMAC-SHA256 hash for storage (plaintext never stored)
// - Display prefix for UI identification.
//
//nolint:nonamedreturns // Named returns improve readability for multiple return values.
func GenerateAPIKey() (plaintext string, hashData *APIKeyHashData, prefix string, err error) {
	// 1. Generate 32 bytes (256 bits) of cryptographic entropy
	entropy := make([]byte, entropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return "", nil, "", fmt.Errorf("failed to generate entropy: %w", err)
	}

	// 2. Encode to base62 (alphanumeric only, no special characters)
	encoded := encodeBase62(entropy)

	// 3. Construct key with OpenShift AI prefix
	plaintext = KeyPrefix + encoded

	// 4. Compute PBKDF2-HMAC-SHA256 hash for storage
	hashData, err = HashAPIKey(plaintext)
	if err != nil {
		return "", nil, "", fmt.Errorf("failed to hash API key: %w", err)
	}

	// 5. Create display prefix (first 12 chars + ellipsis)
	if len(encoded) >= displayPrefixLength {
		prefix = KeyPrefix + encoded[:displayPrefixLength] + "..."
	} else {
		prefix = KeyPrefix + encoded + "..."
	}

	return plaintext, hashData, prefix, nil
}

// HashAPIKey computes PBKDF2-HMAC-SHA256 hash with constant salt
// This is the canonical hashing function - used by both key creation and validation.
// Uses operator-defined constant salt (acceptable for machine-generated high-entropy keys).
func HashAPIKey(key string) (*APIKeyHashData, error) {
	// Use constant salt for operator deployment
	salt := []byte(ConstantSalt)

	// Compute PBKDF2-HMAC-SHA256
	hash := pbkdf2.Key([]byte(key), salt, Iterations, HashBytes, sha256.New)
	
	// Note: The hex-encoded hash is stored in an immutable Go string.
	// Go strings cannot be reliably cleared from memory by user code.
	// The raw byte slice is cleared below for defense-in-depth, but the
	// string copy in result.Hash remains until garbage collected.
	result := &APIKeyHashData{
		Hash:       hex.EncodeToString(hash),
		Iterations: Iterations,
	}

	// Clear raw bytes (defense-in-depth, but see note above about string limitation)
	for i := range hash {
		hash[i] = 0
	}

	return result, nil
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
