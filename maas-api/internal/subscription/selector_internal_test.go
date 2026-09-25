package subscription

import (
	"errors"
	"testing"
)

func TestFindModelRef(t *testing.T) {
	sub := &subscription{
		Name: "test-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "claude-opus-4-8", Namespace: "llm", ModelName: "claude-opus-4-8"},
			{Name: "llama-external", Namespace: "llm", ModelName: "meta-llama/llama-3-70b"},
			{Name: "plain-model", Namespace: "other"},
		},
	}

	tests := []struct {
		name           string
		requestedModel string
		wantRefName    string // "" means expect nil
	}{
		{"namespace/name form", "llm/claude-opus-4-8", "claude-opus-4-8"},
		{"raw spec.modelName (body-routed)", "claude-opus-4-8", "claude-opus-4-8"},
		{"raw CRD name fallback", "plain-model", "plain-model"},
		{"raw model name containing slash", "meta-llama/llama-3-70b", "llama-external"},
		{"no match", "unknown-model", ""},
		{"namespace/name shaped but unknown", "llm/unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := findModelRef(sub, tt.requestedModel)
			if tt.wantRefName == "" {
				if ref != nil {
					t.Fatalf("findModelRef(%q) = %q, want nil", tt.requestedModel, ref.Name)
				}
				return
			}
			if ref == nil {
				t.Fatalf("findModelRef(%q) = nil, want ref %q", tt.requestedModel, tt.wantRefName)
			}
			if ref.Name != tt.wantRefName {
				t.Fatalf("findModelRef(%q) = %q, want %q", tt.requestedModel, ref.Name, tt.wantRefName)
			}
		})
	}
}

func TestCheckModelHealthBodyRoutedAlias(t *testing.T) {
	// Degraded subscription whose model carries rate limits: the TRLP check
	// must resolve body-routed raw model names to the canonical ref instead
	// of rejecting them as InvalidModelFormat.
	newDegradedSub := func(trlpReady bool) *subscription {
		return &subscription{
			Name:  "degraded-sub",
			Phase: PhaseDegraded,
			ModelRefs: []ModelRefInfo{{
				Name:            "claude-model",
				Namespace:       "llm",
				ModelName:       "claude-opus-4-8",
				TokenRateLimits: []TokenRateLimit{{Limit: 1000, Window: "1m"}},
			}},
			TokenRateLimitStatuses: []TokenRateLimitStatus{{Model: "claude-model", Ready: trlpReady}},
		}
	}

	tests := []struct {
		name           string
		requestedModel string
		trlpReady      bool
		wantReason     string // "" means expect nil error
	}{
		{"alias with ready TRLP allowed", "claude-opus-4-8", true, ""},
		{"namespace/name with ready TRLP allowed", "llm/claude-model", true, ""},
		{"alias with unready TRLP fails closed", "claude-opus-4-8", false, "RateLimitNotEnforced"},
		{"model not in subscription", "unknown-model", true, "InvalidModelFormat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkModelHealth(newDegradedSub(tt.trlpReady), tt.requestedModel)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("checkModelHealth(%q) = %v, want nil", tt.requestedModel, err)
				}
				return
			}
			var unhealthy *ModelUnhealthyError
			if !errors.As(err, &unhealthy) {
				t.Fatalf("checkModelHealth(%q) = %v, want ModelUnhealthyError", tt.requestedModel, err)
			}
			if unhealthy.Reason != tt.wantReason {
				t.Fatalf("checkModelHealth(%q) reason = %q, want %q", tt.requestedModel, unhealthy.Reason, tt.wantReason)
			}
		})
	}
}

// TestCheckModelHealthNoTokenBudget covers a Degraded subscription whose model has neither
// tokenRateLimits nor unlimited. The controller leaves such a model out of the TRLP and
// reports it not ready, so inference must be denied rather than allowed for having no
// limit to check.
func TestCheckModelHealthNoTokenBudget(t *testing.T) {
	sub := &subscription{
		Name:                   "degraded-sub",
		Phase:                  PhaseDegraded,
		ModelRefs:              []ModelRefInfo{{Name: "llm", Namespace: "ns"}},
		TokenRateLimitStatuses: []TokenRateLimitStatus{{Model: "llm", Ready: false, Reason: "InvalidSpec"}},
	}
	err := checkModelHealth(sub, "ns/llm")
	var unhealthy *ModelUnhealthyError
	if !errors.As(err, &unhealthy) || unhealthy.Reason != "RateLimitNotEnforced" {
		t.Fatalf("checkModelHealth(ns/llm) = %v, want RateLimitNotEnforced", err)
	}
}

// TestCheckModelHealthUnenforceableSpec covers an Active subscription whose model has a
// token budget the controller cannot enforce, as seen while the Degraded status has not
// been written: during a rolling upgrade, or when the API server rejects the write.
// maas-api must deny it from the spec rather than trust the phase.
func TestCheckModelHealthUnenforceableSpec(t *testing.T) {
	tests := []struct {
		name string
		ref  ModelRefInfo
	}{
		{"window over 366 days", ModelRefInfo{Name: "llm", Namespace: "ns", TokenRateLimits: []TokenRateLimit{{Limit: 1000, Window: "9999h"}}}},
		{"no token budget", ModelRefInfo{Name: "llm", Namespace: "ns"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub := &subscription{
				Name:                   "active-sub",
				Phase:                  PhaseActive,
				ModelRefs:              []ModelRefInfo{tt.ref},
				TokenRateLimitStatuses: []TokenRateLimitStatus{{Model: "llm", Ready: true, Reason: "Accepted"}},
			}
			err := checkModelHealth(sub, "ns/llm")
			var unhealthy *ModelUnhealthyError
			if !errors.As(err, &unhealthy) || unhealthy.Reason != "RateLimitNotEnforced" {
				t.Fatalf("checkModelHealth(ns/llm) = %v, want RateLimitNotEnforced", err)
			}
		})
	}
}

func TestValidTokenBudget(t *testing.T) {
	limits := func(windows ...string) []TokenRateLimit {
		out := make([]TokenRateLimit, 0, len(windows))
		for _, w := range windows {
			out = append(out, TokenRateLimit{Limit: 1000, Window: w})
		}
		return out
	}
	tests := []struct {
		name string
		ref  ModelRefInfo
		want bool
	}{
		{"seconds at the pattern maximum", ModelRefInfo{TokenRateLimits: limits("9999s")}, true},
		{"minutes at the pattern maximum", ModelRefInfo{TokenRateLimits: limits("9999m")}, true},
		{"hours below the cap", ModelRefInfo{TokenRateLimits: limits("1000h")}, true},
		{"366 days", ModelRefInfo{TokenRateLimits: limits("8784h")}, true},
		{"one hour past 366 days", ModelRefInfo{TokenRateLimits: limits("8785h")}, false},
		{"9999h", ModelRefInfo{TokenRateLimits: limits("9999h")}, false},
		{"days unit", ModelRefInfo{TokenRateLimits: limits("1d")}, false},
		{"leading zero", ModelRefInfo{TokenRateLimits: limits("01h")}, false},
		{"one invalid limit among valid ones", ModelRefInfo{TokenRateLimits: limits("1m", "9999h")}, false},
		{"zero limit", ModelRefInfo{TokenRateLimits: []TokenRateLimit{{Limit: 0, Window: "1m"}}}, false},
		{"limit at the maximum", ModelRefInfo{TokenRateLimits: []TokenRateLimit{{Limit: maxTokenRateLimit, Window: "1m"}}}, true},
		{"limit past the maximum", ModelRefInfo{TokenRateLimits: []TokenRateLimit{{Limit: maxTokenRateLimit + 1, Window: "1m"}}}, false},
		{"no budget", ModelRefInfo{}, false},
		{"unlimited", ModelRefInfo{Unlimited: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validTokenBudget(tt.ref); got != tt.want {
				t.Errorf("validTokenBudget(%+v) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestEnforceableModelDuplicateEntries(t *testing.T) {
	sub := &subscription{ModelRefs: []ModelRefInfo{
		{Name: "llm", Namespace: "ns", TokenRateLimits: []TokenRateLimit{{Limit: 1000, Window: "9999h"}}},
		{Name: "llm", Namespace: "ns", TokenRateLimits: []TokenRateLimit{{Limit: 1000, Window: "1h"}}},
		{Name: "llm", Namespace: "other"},
	}}
	if !enforceableModel(sub, &sub.ModelRefs[0]) {
		t.Errorf("ns/llm: want enforceable through its second, valid entry")
	}
	if enforceableModel(sub, &sub.ModelRefs[2]) {
		t.Errorf("other/llm: want unenforceable, it has no token budget")
	}
}
