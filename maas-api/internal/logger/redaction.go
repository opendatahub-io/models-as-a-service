package logger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/textproto"
	"sync"
)

var (
	// SensitiveHeaders lists HTTP headers that must never be logged in full.
	// Header names are canonicalized (e.g., "authorization" → "Authorization").
	SensitiveHeaders = []string{
		"Authorization",
		"X-Api-Key",
		"Cookie",
		"Set-Cookie",
	}

	// hmacKey is a per-process random key used to prevent rainbow table attacks
	// on redacted header hash prefixes.
	hmacKey     []byte
	hmacKeyOnce sync.Once
)

// initHMACKey initializes the per-process HMAC key for header redaction.
// Panics if random key generation fails (which should never happen on modern systems).
func initHMACKey() {
	hmacKeyOnce.Do(func() {
		hmacKey = make([]byte, 32)
		if _, err := rand.Read(hmacKey); err != nil {
			// crypto/rand.Read never fails on Linux/modern systems.
			// If it does, something is seriously wrong - panic rather than
			// silently falling back to a deterministic key that defeats rainbow table protection.
			panic("failed to generate HMAC key for header redaction: " + err.Error())
		}
	})
}

// RedactHeader returns a safe representation of a header value for logging.
// - Empty values return "absent"
// - Non-empty values return "present" by default
// - If hashPrefix is true, returns "present:sha256:<first-8-chars-of-hmac>".
// Uses HMAC-SHA256 with a per-process random key to prevent rainbow table attacks.
func RedactHeader(value string, hashPrefix bool) string {
	if value == "" {
		return "absent"
	}

	if !hashPrefix {
		return "present"
	}

	// Initialize HMAC key if not already done
	initHMACKey()

	// HMAC-SHA256 the value with per-process key to prevent rainbow tables
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(value))
	digest := mac.Sum(nil)
	encoded := base64.URLEncoding.EncodeToString(digest)
	return "present:sha256:" + encoded[:8]
}

// RedactHeaders returns a map of header names to safe-to-log values.
// Redacts sensitive headers, passes through others unchanged.
// Header matching is case-insensitive using canonical MIME header names.
func RedactHeaders(headers http.Header, hashPrefix bool) map[string]string {
	result := make(map[string]string)

	// Build canonical sensitive header set
	sensitive := make(map[string]bool)
	for _, h := range SensitiveHeaders {
		canonical := textproto.CanonicalMIMEHeaderKey(h)
		sensitive[canonical] = true
	}

	for name, values := range headers {
		// Canonicalize header name for case-insensitive matching
		canonical := textproto.CanonicalMIMEHeaderKey(name)

		if sensitive[canonical] {
			// Redact sensitive headers
			val := ""
			if len(values) > 0 {
				val = values[0]
			}
			result[name] = RedactHeader(val, hashPrefix)
		} else if len(values) > 0 {
			// Pass through non-sensitive headers
			result[name] = values[0]
		}
	}

	return result
}

// IsSensitiveHeader checks if a header name is in the sensitive list.
// Matching is case-insensitive using canonical MIME header names.
func IsSensitiveHeader(name string) bool {
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	for _, h := range SensitiveHeaders {
		if textproto.CanonicalMIMEHeaderKey(h) == canonical {
			return true
		}
	}
	return false
}
