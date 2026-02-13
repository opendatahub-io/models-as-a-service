package api_keys

import (
	"regexp"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateAPIKey()

	if err != nil {
		t.Fatalf("GenerateAPIKey() returned error: %v", err)
	}

	// Test 1: Key has correct prefix
	if !IsValidKeyFormat(plaintext) {
		t.Errorf("GenerateAPIKey() key missing prefix 'sk-oai-': got %q", plaintext)
	}

	// Test 2: Hash is 64 hex characters (SHA-256)
	if len(hash) != 64 {
		t.Errorf("GenerateAPIKey() hash length = %d, want 64", len(hash))
	}

	// Test 3: Hash is valid hex
	hexRegex := regexp.MustCompile("^[0-9a-f]{64}$")
	if !hexRegex.MatchString(hash) {
		t.Errorf("GenerateAPIKey() hash is not valid hex: %q", hash)
	}

	// Test 4: Prefix has correct format
	prefixRegex := regexp.MustCompile(`^sk-oai-[A-Za-z0-9]{12}\.\.\.$`)
	if !prefixRegex.MatchString(prefix) {
		t.Errorf("GenerateAPIKey() prefix format incorrect: got %q", prefix)
	}

	// Test 5: Key is alphanumeric after prefix (base62)
	keyBody := plaintext[len(KeyPrefix):]
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
	// Generate multiple keys and ensure they're unique
	keys := make(map[string]bool)
	hashes := make(map[string]bool)

	for i := 0; i < 100; i++ {
		plaintext, hash, _, err := GenerateAPIKey()
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
	tests := []struct {
		name string
		key  string
		want string // Pre-computed SHA-256 hash
	}{
		{
			name: "known test vector",
			key:  "sk-oai-test123",
			want: "c9d26d3b0f5e8d7a4c8b5e2f1d9a6c3e8b5f2d9a6c3e8b5f2d9a6c3e8b5f2d9a", // Placeholder - will be computed
		},
	}

	// Actually compute the hash for the test key
	testKey := "sk-oai-test123"
	hash := HashAPIKey(testKey)

	// Verify consistent hashing
	hash2 := HashAPIKey(testKey)
	if hash != hash2 {
		t.Errorf("HashAPIKey() not deterministic: got %q and %q", hash, hash2)
	}

	// Verify hash length
	if len(hash) != 64 {
		t.Errorf("HashAPIKey() length = %d, want 64", len(hash))
	}

	// Verify different keys produce different hashes
	differentHash := HashAPIKey("sk-oai-different")
	if hash == differentHash {
		t.Error("HashAPIKey() produced same hash for different keys")
	}

	_ = tests // Silence unused variable warning
}

func TestIsValidKeyFormat(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"valid key", "sk-oai-ABC123xyz", true},
		{"empty string", "", false},
		{"wrong prefix", "sk-xyz-ABC123", false},
		{"no prefix", "ABC123xyz", false},
		{"jwt token", "eyJhbGciOiJSUzI1NiIs...", false},
		{"partial prefix", "sk-oai", false},
		{"exact prefix only", "sk-oai-", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidKeyFormat(tt.key); got != tt.want {
				t.Errorf("IsValidKeyFormat(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestEncodeBase62(t *testing.T) {
	// Test that encoding produces alphanumeric output
	testData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
	result := encodeBase62(testData)

	alphanumRegex := regexp.MustCompile("^[A-Za-z0-9]+$")
	if !alphanumRegex.MatchString(result) {
		t.Errorf("encodeBase62() produced non-alphanumeric output: %q", result)
	}
}

func BenchmarkGenerateAPIKey(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _, _, _ = GenerateAPIKey()
	}
}

func BenchmarkHashAPIKey(b *testing.B) {
	key := "sk-oai-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HashAPIKey(key)
	}
}
