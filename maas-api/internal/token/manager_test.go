package token

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSanitizeServiceAccountName(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	username := "Alice@example.com"

	name, err := manager.sanitizeServiceAccountName(username)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	if len(name) > 63 {
		t.Errorf("expected name length <= 63, got %d", len(name))
	}

	if !strings.HasPrefix(name, "alice-example-com-") {
		t.Errorf("expected name to start with sanitized base, got %q", name)
	}

	suffix := name[strings.LastIndex(name, "-")+1:]
	if len(suffix) != 12 {
		t.Errorf("expected 12-char suffix, got %d (%q)", len(suffix), suffix)
	}

	sum := sha256.Sum256([]byte(username))
	expectedSuffix := hex.EncodeToString(sum[:])[:12]
	if suffix != expectedSuffix {
		t.Errorf("expected suffix %q, got %q", expectedSuffix, suffix)
	}
}

func TestSanitizeServiceAccountName_InvalidUsername(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	if _, err := manager.sanitizeServiceAccountName("!!!"); err == nil {
		t.Error("expected error for invalid username, got nil")
	}
}

func TestSanitizeServiceAccountName_Truncation(t *testing.T) {
	t.Parallel()

	manager := &Manager{}
	username := strings.Repeat("a", 200) + "@example.com"

	name, err := manager.sanitizeServiceAccountName(username)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	if len(name) > 63 {
		t.Errorf("expected name length <= 63, got %d", len(name))
	}

	sum := sha256.Sum256([]byte(username))
	expectedSuffix := hex.EncodeToString(sum[:])[:12]
	if !strings.HasSuffix(name, "-"+expectedSuffix) {
		t.Errorf("expected name to end with sha256 suffix, got %q", name)
	}
}
