package api_keys_test

import (
	"regexp"
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hashData, prefix, err := api_keys.GenerateAPIKey()

	if err != nil {
		t.Fatalf("GenerateAPIKey() returned error: %v", err)
	}

	// Test 1: Key has correct prefix
	if !api_keys.IsValidKeyFormat(plaintext) {
		t.Errorf("GenerateAPIKey() key missing prefix 'sk-oai-': got %q", plaintext)
	}

	// Test 2: PBKDF2 hash is 64 hex characters (32 bytes)
	if len(hashData.Hash) != 64 {
		t.Errorf("GenerateAPIKey() hash length = %d, want 64", len(hashData.Hash))
	}

	// Test 3: Hash is valid hex
	hexRegex := regexp.MustCompile("^[0-9a-f]{64}$")
	if !hexRegex.MatchString(hashData.Hash) {
		t.Errorf("GenerateAPIKey() hash is not valid hex: %q", hashData.Hash)
	}

	// Test 4: Iterations are correct
	if hashData.Iterations != 600000 {
		t.Errorf("GenerateAPIKey() iterations = %d, want 600000", hashData.Iterations)
	}

	// Test 7: Prefix has correct format
	prefixRegex := regexp.MustCompile(`^sk-oai-[A-Za-z0-9]{12}\.\.\.$`)
	if !prefixRegex.MatchString(prefix) {
		t.Errorf("GenerateAPIKey() prefix format incorrect: got %q", prefix)
	}

	// Test 8: Key is alphanumeric after prefix (base62)
	keyBody := plaintext[len(api_keys.KeyPrefix):]
	alphanumRegex := regexp.MustCompile("^[A-Za-z0-9]+$")
	if !alphanumRegex.MatchString(keyBody) {
		t.Errorf("GenerateAPIKey() key body not alphanumeric: got %q", keyBody)
	}

	// Test 9: Key body is sufficiently long (256 bits → ~43 base62 chars)
	if len(keyBody) < 40 {
		t.Errorf("GenerateAPIKey() key body too short: got %d chars, want >= 40", len(keyBody))
	}
}

func TestGenerateAPIKey_Uniqueness(t *testing.T) {
	keys := make(map[string]bool)
	hashes := make(map[string]bool)

	for i := range 3 {
		plaintext, hashData, _, err := api_keys.GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() iteration %d returned error: %v", i, err)
		}

		if keys[plaintext] {
			t.Errorf("GenerateAPIKey() generated duplicate key on iteration %d", i)
		}
		keys[plaintext] = true

		if hashes[hashData.Hash] {
			t.Errorf("GenerateAPIKey() generated duplicate hash on iteration %d", i)
		}
		hashes[hashData.Hash] = true
	}
}

func TestHashAPIKey(t *testing.T) {
	// Compute the PBKDF2 hash for the test key
	testKey := "sk-oai-test123"
	hashData, err := api_keys.HashAPIKey(testKey)
	if err != nil {
		t.Fatalf("HashAPIKey() returned error: %v", err)
	}

	// Verify hash length (32 bytes = 64 hex chars)
	if len(hashData.Hash) != 64 {
		t.Errorf("HashAPIKey() hash length = %d, want 64", len(hashData.Hash))
	}


	// Verify iterations
	if hashData.Iterations != 600000 {
		t.Errorf("HashAPIKey() iterations = %d, want 600000", hashData.Iterations)
	}

	// Verify different keys produce different hashes (with constant salt)
	differentHashData, err := api_keys.HashAPIKey("sk-oai-different")
	if err != nil {
		t.Fatalf("HashAPIKey() for different key returned error: %v", err)
	}
	if hashData.Hash == differentHashData.Hash {
		t.Error("HashAPIKey() produced same hash for different keys")
	}
}

func TestIsValidKeyFormat(t *testing.T) {
	t.Run("ValidKey", func(t *testing.T) {
		if !api_keys.IsValidKeyFormat("sk-oai-ABC123xyz") {
			t.Error("Valid key should pass")
		}
	})

	t.Run("InvalidKey", func(t *testing.T) {
		if api_keys.IsValidKeyFormat("invalid-key") {
			t.Error("Invalid key should fail")
		}
	})
}

func BenchmarkGenerateAPIKey(b *testing.B) {
	for b.Loop() {
		_, _, _, _ = api_keys.GenerateAPIKey()
	}
}

func TestHashComparison(t *testing.T) {
	key := "sk-oai-test-verification"
	
	// Hash the key
	hashData, err := api_keys.HashAPIKey(key)
	if err != nil {
		t.Fatalf("HashAPIKey() returned error: %v", err)
	}

	// Test correct key verification using direct hash comparison
	correctHash, err := api_keys.HashAPIKey(key)
	if err != nil {
		t.Errorf("HashAPIKey() returned error: %v", err)
	}
	if correctHash.Hash != hashData.Hash {
		t.Error("Hash comparison should return true for correct key")
	}

	// Test wrong key verification using direct hash comparison
	wrongHash, err := api_keys.HashAPIKey("sk-oai-wrong-key")
	if err != nil {
		t.Errorf("HashAPIKey() returned error for wrong key: %v", err)
	}
	if wrongHash.Hash == hashData.Hash {
		t.Error("Hash comparison should return false for wrong key")
	}
}

func BenchmarkHashAPIKey(b *testing.B) {
	key := "sk-oai-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"

	b.ResetTimer()
	for b.Loop() {
		_, _ = api_keys.HashAPIKey(key)
	}
}

func BenchmarkHashComparison(b *testing.B) {
	key := "sk-oai-benchmark-verification-test"
	hashData, _ := api_keys.HashAPIKey(key)

	b.ResetTimer()
	for b.Loop() {
		computed, _ := api_keys.HashAPIKey(key)
		_ = computed.Hash == hashData.Hash
	}
}
