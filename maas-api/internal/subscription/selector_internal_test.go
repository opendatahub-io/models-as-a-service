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
