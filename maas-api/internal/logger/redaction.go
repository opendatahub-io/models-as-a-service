package logger

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"slices"
)

// SensitiveHeaders lists HTTP headers that must never be logged in full.
var SensitiveHeaders = []string{
	"Authorization",
	"X-API-Key",
	"Cookie",
	"Set-Cookie",
}

// RedactHeader returns a safe representation of a header value for logging.
// - Empty values return "absent"
// - Non-empty values return "present" by default
// - If hashPrefix is true, returns "present:sha256:<first-8-chars-of-hash>".
func RedactHeader(value string, hashPrefix bool) string {
	if value == "" {
		return "absent"
	}

	if !hashPrefix {
		return "present"
	}

	// SHA256 hash the value and return first 8 chars for correlation
	hash := sha256.Sum256([]byte(value))
	encoded := base64.URLEncoding.EncodeToString(hash[:])
	return "present:sha256:" + encoded[:8]
}

// RedactHeaders returns a map of header names to safe-to-log values.
// Redacts sensitive headers, passes through others unchanged.
func RedactHeaders(headers http.Header, hashPrefix bool) map[string]string {
	result := make(map[string]string)

	sensitive := make(map[string]bool)
	for _, h := range SensitiveHeaders {
		sensitive[h] = true
	}

	for name, values := range headers {
		if sensitive[name] {
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
func IsSensitiveHeader(name string) bool {
	return slices.Contains(SensitiveHeaders, name)
}
