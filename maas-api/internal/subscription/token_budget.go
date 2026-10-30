package subscription

import (
	"regexp"
	"strconv"
)

// Token budget bounds. These must match validateTokenRateLimit in
// maas-controller/pkg/controller/maas/maassubscription_controller.go, which leaves any
// model reference outside them out of the TRLP.
const (
	maxTokenRateLimit int64 = 1_000_000_000
	maxWindowSeconds  int64 = 366 * 24 * 3600
)

var windowPattern = regexp.MustCompile(`^[1-9]\d{0,3}(s|m|h)$`)

var windowUnitSeconds = map[byte]int64{'s': 1, 'm': 60, 'h': 3600}

// enforceableModel reports whether the controller puts the subscription into the TRLP of
// ref's model: some entry for that model has a valid token budget. A subscription can
// list the same model more than once; the controller enforces the first valid entry.
func enforceableModel(sub *subscription, ref *ModelRefInfo) bool {
	for _, r := range sub.ModelRefs {
		if r.Namespace == ref.Namespace && r.Name == ref.Name && validTokenBudget(r) {
			return true
		}
	}
	return false
}

// validTokenBudget reports whether a model reference is unlimited or has token rate
// limits that are all within bounds. Declared limits win over unlimited, as in
// parseModelRef.
func validTokenBudget(ref ModelRefInfo) bool {
	if len(ref.TokenRateLimits) == 0 {
		return ref.Unlimited
	}
	for _, trl := range ref.TokenRateLimits {
		if !validTokenRateLimit(trl) {
			return false
		}
	}
	return true
}

func validTokenRateLimit(trl TokenRateLimit) bool {
	if trl.Limit < 1 || trl.Limit > maxTokenRateLimit || !windowPattern.MatchString(trl.Window) {
		return false
	}
	value, err := strconv.ParseInt(trl.Window[:len(trl.Window)-1], 10, 64)
	if err != nil {
		return false
	}
	return value*windowUnitSeconds[trl.Window[len(trl.Window)-1]] <= maxWindowSeconds
}
