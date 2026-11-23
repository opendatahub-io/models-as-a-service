package tenantreconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// BoundedName builds a stable bounded name from supplied framing and value.
//
// It preserves a fitting name unchanged; otherwise it retains a value prefix and appends a
// digest of the complete value.
//
// Parameters:
//   - prefix: text preceding the value in the generated name.
//   - value: variable text retained or hashed to distinguish the generated name.
//   - suffix: text following the value in the generated name.
//   - maxLength: maximum byte length allowed for the generated name.
//
// Returns a name no longer than maxLength when the supplied prefix and suffix allow it.
func BoundedName(prefix, value, suffix string, maxLength int) string {
	name := prefix + value + suffix
	if len(name) <= maxLength {
		return name
	}
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:])[:8]
	budget := maxLength - len(prefix) - len(suffix) - len(hash) - 1
	if budget < 1 {
		fallback := strings.TrimSuffix(prefix, "-") + "-" + hash + suffix
		if len(fallback) <= maxLength {
			return fallback
		}
		return strings.TrimSuffix(prefix, "-") + "-" + hash
	}
	trimmed := strings.Trim(value[:budget], "-.")
	if trimmed == "" {
		trimmed = hash
	}
	return prefix + trimmed + suffix + "-" + hash
}
