package api_keys_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
)

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := api_keys.GenerateAPIKey()

	if err != nil {
		t.Fatalf("GenerateAPIKey() returned error: %v", err)
	}

	// Test 1: Key has correct format (sk-oai-{key_id}_{secret})
	if !api_keys.IsValidKeyFormat(plaintext) {
		t.Errorf("GenerateAPIKey() key has invalid format: got %q", plaintext)
	}

	// Extract key_id using ParseAPIKey for further tests
	keyID, _, parseErr := api_keys.ParseAPIKey(plaintext)
	if parseErr != nil {
		t.Fatalf("ParseAPIKey() failed on generated key: %v", parseErr)
	}

	// Test 2: Key contains the key_id
	if !strings.Contains(plaintext, keyID) {
		t.Errorf("GenerateAPIKey() key should contain key_id %q: got %q", keyID, plaintext)
	}

	// Test 3: Key has correct structure (prefix + key_id + separator + secret)
	if !strings.HasPrefix(plaintext, api_keys.KeyPrefix+keyID+api_keys.KeyIDSeparator) {
		t.Errorf("GenerateAPIKey() key should have format sk-oai-{key_id}_{secret}: got %q", plaintext)
	}

	// Test 4: Hash is 64 hex characters (SHA-256)
	if len(hash) != 64 {
		t.Errorf("GenerateAPIKey() hash length = %d, want 64", len(hash))
	}

	// Test 5: Hash is valid hex
	hexRegex := regexp.MustCompile("^[0-9a-f]{64}$")
	if !hexRegex.MatchString(hash) {
		t.Errorf("GenerateAPIKey() hash is not valid hex: %q", hash)
	}

	// Test 6: Prefix has correct format (shows first 12 chars of key_id)
	// Note: 96-bit key_id encodes to ~16 base62 chars (log62(2^96) ≈ 16.1), so key_id
	// is always >= 12 chars, making the displayPrefixLength truncation always apply.
	prefixRegex := regexp.MustCompile(`^sk-oai-[A-Za-z0-9]{12}\.\.\.$`)
	if !prefixRegex.MatchString(prefix) {
		t.Errorf("GenerateAPIKey() prefix format incorrect: got %q", prefix)
	}

	// Test 7: key_id is base62 and expected length (~16 chars from 96 bits)
	alphanumRegex := regexp.MustCompile("^[A-Za-z0-9]+$")
	if !alphanumRegex.MatchString(keyID) {
		t.Errorf("GenerateAPIKey() key_id not alphanumeric: got %q", keyID)
	}
	// 96 bits of entropy → log62(2^96) ≈ 16.1 chars, so key_id should be 15-17 chars
	if len(keyID) < 12 {
		t.Errorf("GenerateAPIKey() key_id too short: got %d chars, want >= 12", len(keyID))
	}
}

func TestGenerateAPIKey_Uniqueness(t *testing.T) {
	// Generate multiple keys and ensure they're unique
	keys := make(map[string]bool)
	keyIDs := make(map[string]bool)
	hashes := make(map[string]bool)

	for i := range 100 {
		plaintext, hash, _, err := api_keys.GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey() iteration %d returned error: %v", i, err)
		}

		keyID, _, parseErr := api_keys.ParseAPIKey(plaintext)
		if parseErr != nil {
			t.Fatalf("ParseAPIKey() iteration %d returned error: %v", i, parseErr)
		}

		if keys[plaintext] {
			t.Errorf("GenerateAPIKey() generated duplicate key on iteration %d", i)
		}
		keys[plaintext] = true

		if keyIDs[keyID] {
			t.Errorf("GenerateAPIKey() generated duplicate key_id on iteration %d", i)
		}
		keyIDs[keyID] = true

		if hashes[hash] {
			t.Errorf("GenerateAPIKey() generated duplicate hash on iteration %d", i)
		}
		hashes[hash] = true
	}
}

func TestHashAPIKey(t *testing.T) {
	// Use the new key format: sk-oai-{key_id}_{secret}
	testKey := "sk-oai-testKeyID123_testSecretValue456"
	hash := api_keys.HashAPIKey(testKey)

	// Verify consistent hashing
	hash2 := api_keys.HashAPIKey(testKey)
	if hash != hash2 {
		t.Errorf("HashAPIKey() not deterministic: got %q and %q", hash, hash2)
	}

	// Verify hash length
	if len(hash) != 64 {
		t.Errorf("HashAPIKey() length = %d, want 64", len(hash))
	}

	// Verify different keys produce different hashes
	differentHash := api_keys.HashAPIKey("sk-oai-differentID_differentSecret")
	if hash == differentHash {
		t.Error("HashAPIKey() produced same hash for different keys")
	}

	// Verify invalid key format returns empty hash
	invalidHash := api_keys.HashAPIKey("invalid-key-no-separator")
	if invalidHash != "" {
		t.Errorf("HashAPIKey() should return empty string for invalid format, got %q", invalidHash)
	}
}

func TestParseAPIKey(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		wantKeyID   string
		wantSecret  string
		wantErr     bool
	}{
		{
			name:       "valid key",
			key:        "sk-oai-myKeyID123_mySecretValue456",
			wantKeyID:  "myKeyID123",
			wantSecret: "mySecretValue456",
			wantErr:    false,
		},
		{
			name:    "missing prefix",
			key:     "myKeyID123_mySecretValue456",
			wantErr: true,
		},
		{
			name:    "missing separator",
			key:     "sk-oai-noSeparatorHere",
			wantErr: true,
		},
		{
			name:    "empty key_id",
			key:     "sk-oai-_onlySecret",
			wantErr: true,
		},
		{
			name:    "empty secret",
			key:     "sk-oai-onlyKeyID_",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyID, secret, err := api_keys.ParseAPIKey(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseAPIKey() expected error, got keyID=%q secret=%q", keyID, secret)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseAPIKey() unexpected error: %v", err)
				return
			}
			if keyID != tt.wantKeyID {
				t.Errorf("ParseAPIKey() keyID = %q, want %q", keyID, tt.wantKeyID)
			}
			if secret != tt.wantSecret {
				t.Errorf("ParseAPIKey() secret = %q, want %q", secret, tt.wantSecret)
			}
		})
	}
}

func TestValidateAPIKeyHash(t *testing.T) {
	// Generate a key and verify the hash validation works
	plaintext, hash, _, err := api_keys.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() returned error: %v", err)
	}

	// Valid key should validate successfully
	if !api_keys.ValidateAPIKeyHash(plaintext, hash) {
		t.Error("ValidateAPIKeyHash() should return true for valid key")
	}

	// Wrong hash should fail
	if api_keys.ValidateAPIKeyHash(plaintext, "wronghash") {
		t.Error("ValidateAPIKeyHash() should return false for wrong hash")
	}

	// Wrong key should fail
	if api_keys.ValidateAPIKeyHash("sk-oai-wrong_key", hash) {
		t.Error("ValidateAPIKeyHash() should return false for wrong key")
	}

	// Invalid key format should fail
	if api_keys.ValidateAPIKeyHash("invalid-key", hash) {
		t.Error("ValidateAPIKeyHash() should return false for invalid key format")
	}
}

func TestIsValidKeyFormat(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		valid bool
	}{
		{"valid key with separator", "sk-oai-keyID123_secretValue456", true},
		{"missing prefix", "keyID123_secretValue456", false},
		{"missing separator", "sk-oai-noSeparatorHere", false},
		{"empty key_id", "sk-oai-_onlySecret", false},
		{"empty secret", "sk-oai-onlyKeyID_", false},
		{"invalid chars in key_id", "sk-oai-key-ID_secret", false},
		{"invalid chars in secret", "sk-oai-keyID_sec-ret", false},
		{"completely invalid", "invalid-key", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := api_keys.IsValidKeyFormat(tt.key)
			if got != tt.valid {
				t.Errorf("IsValidKeyFormat(%q) = %v, want %v", tt.key, got, tt.valid)
			}
		})
	}
}

func BenchmarkGenerateAPIKey(b *testing.B) {
	for b.Loop() {
		_, _, _, _ = api_keys.GenerateAPIKey()
	}
}

func BenchmarkHashAPIKey(b *testing.B) {
	key := "sk-oai-testKeyID123456_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefgh"

	for b.Loop() {
		_ = api_keys.HashAPIKey(key)
	}
}

func BenchmarkValidateAPIKeyHash(b *testing.B) {
	plaintext, hash, _, err := api_keys.GenerateAPIKey()
	if err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		_ = api_keys.ValidateAPIKeyHash(plaintext, hash)
	}
}
