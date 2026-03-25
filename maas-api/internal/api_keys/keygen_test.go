package api_keys_test

import (
	"regexp"
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
)

const testAPIKey = "sk-oai-test123"
const testSalt = "this-is-a-test-salt-that-is-at-least-32-characters-long"

func setupTestSalt(tb testing.TB) {
	tb.Helper()
	if err := api_keys.SetHashSalt(testSalt); err != nil {
		tb.Fatalf("failed to set test hash salt: %v", err)
	}
}

func TestGenerateAPIKey(t *testing.T) {
	setupTestSalt(t)
	plaintext, hash, prefix, err := api_keys.GenerateAPIKey()

	if err != nil {
		t.Fatalf("GenerateAPIKey() returned error: %v", err)
	}

	// Test 1: Key has correct prefix
	if !api_keys.IsValidKeyFormat(plaintext) {
		t.Errorf("GenerateAPIKey() key missing prefix 'sk-oai-': got %q", plaintext)
	}

	// Test 2: Hash is 64 hex chars (SHA-256 output)
	hashRegex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hashRegex.MatchString(hash) {
		t.Errorf("GenerateAPIKey() hash not in expected format (64 hex chars): got %q", hash)
	}

	// Test 4: Prefix has correct format
	prefixRegex := regexp.MustCompile(`^sk-oai-[A-Za-z0-9]{12}\.\.\.$`)
	if !prefixRegex.MatchString(prefix) {
		t.Errorf("GenerateAPIKey() prefix format incorrect: got %q", prefix)
	}

	// Test 5: Key is alphanumeric after prefix (base62)
	keyBody := plaintext[len(api_keys.KeyPrefix):]
	alphanumRegex := regexp.MustCompile("^[A-Za-z0-9]+$")
	if !alphanumRegex.MatchString(keyBody) {
		t.Errorf("GenerateAPIKey() key body not alphanumeric: got %q", keyBody)
	}

	// Test 6: Key body is sufficiently long (256 bits → ~43 base62 chars)
	if len(keyBody) < 40 {
		t.Errorf("GenerateAPIKey() key body too short: got %d chars, want >= 40", len(keyBody))
	}
}

func TestGenerateAPIKey_Uniqueness(t *testing.T) {
	setupTestSalt(t)
	// Generate multiple keys and ensure they're unique
	keys := make(map[string]bool)
	hashes := make(map[string]bool)

	for i := range 100 {
		plaintext, hash, _, err := api_keys.GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() iteration %d returned error: %v", i, err)
		}

		if keys[plaintext] {
			t.Errorf("GenerateAPIKey() generated duplicate key on iteration %d", i)
		}
		keys[plaintext] = true

		if hashes[hash] {
			t.Errorf("GenerateAPIKey() generated duplicate hash on iteration %d", i)
		}
		hashes[hash] = true
	}
}

func TestHashAPIKey(t *testing.T) {
	setupTestSalt(t)
	testKey := testAPIKey

	// Test 1: Function returns hash
	hash1 := api_keys.HashAPIKey(testKey)

	// Test 2: Hash has correct format (64 hex chars)
	hashRegex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hashRegex.MatchString(hash1) {
		t.Errorf("HashAPIKey() hash not in expected format: got %q", hash1)
	}

	// Test 3: Same key produces same hash (deterministic with constant salt)
	hash2 := api_keys.HashAPIKey(testKey)
	if hash1 != hash2 {
		t.Error("HashAPIKey() should produce same hash for same key (constant salt)")
	}

	// Test 4: Different keys produce different hashes
	differentHash := api_keys.HashAPIKey("sk-oai-different")
	if hash1 == differentHash {
		t.Error("HashAPIKey() should produce different hashes for different keys")
	}
}

func TestValidateAPIKeyHash(t *testing.T) {
	setupTestSalt(t)
	testKey := "sk-oai-test123"

	t.Run("ValidateHash", func(t *testing.T) {
		// Create hash using constant salt
		hash := api_keys.HashAPIKey(testKey)

		// Should validate successfully
		if !api_keys.ValidateAPIKeyHash(testKey, hash) {
			t.Error("ValidateAPIKeyHash() should validate correct hash")
		}

		// Should fail with wrong key
		if api_keys.ValidateAPIKeyHash("sk-oai-wrong", hash) {
			t.Error("ValidateAPIKeyHash() should reject wrong key")
		}
	})

	t.Run("RejectInvalidFormats", func(t *testing.T) {
		testCases := []string{
			"invalid-hash",
			"short",
			"not-a-valid-hex-string-that-is-64-characters-long-xxxxxxxxxx",
			// Wrong length (not 64 hex chars)
			"abc123",
			"0123456789abcdef0123456789abcdef", // 32 chars, not 64
			// Contains colons (old format)
			"abc:def",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef:extra",
		}

		for _, invalidHash := range testCases {
			if api_keys.ValidateAPIKeyHash(testKey, invalidHash) {
				t.Errorf("ValidateAPIKeyHash() should reject invalid format: %q", invalidHash)
			}
		}
	})
}

func TestConstantTimeCompare(t *testing.T) {
	setupTestSalt(t)
	// This test verifies that our constant-time comparison works correctly
	// Note: We can't easily test the timing properties in a unit test

	testKey := "sk-oai-test123"

	// Create hash (deterministic with constant salt)
	hash := api_keys.HashAPIKey(testKey)

	// Should validate the same key
	if !api_keys.ValidateAPIKeyHash(testKey, hash) {
		t.Error("ValidateAPIKeyHash() should validate correct key")
	}

	// Should reject wrong key
	if api_keys.ValidateAPIKeyHash("sk-oai-different", hash) {
		t.Error("ValidateAPIKeyHash() should reject wrong key")
	}

	// Should reject slightly different key (timing attack test)
	if api_keys.ValidateAPIKeyHash("sk-oai-test124", hash) {
		t.Error("ValidateAPIKeyHash() should reject similar but different key")
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
	setupTestSalt(b)
	for b.Loop() {
		_, _, _, _ = api_keys.GenerateAPIKey()
	}
}

func BenchmarkHashAPIKey(b *testing.B) {
	setupTestSalt(b)
	key := "sk-oai-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"

	b.ResetTimer()
	for b.Loop() {
		_ = api_keys.HashAPIKey(key)
	}
}

func BenchmarkValidateAPIKeyHash(b *testing.B) {
	setupTestSalt(b)
	key := "sk-oai-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"

	// Create hash for benchmark
	hash := api_keys.HashAPIKey(key)

	b.ResetTimer()
	for b.Loop() {
		_ = api_keys.ValidateAPIKeyHash(key, hash)
	}
}
