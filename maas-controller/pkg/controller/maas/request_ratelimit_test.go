/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"strings"
	"testing"
)

func TestBuildRequestCountGroupedLimits_predicateIncludesRerankPath(t *testing.T) {
	limits, subNames := buildRequestCountGroupedLimits([]subInfo{
		{
			subNamespace: "models-as-a-service",
			subName:      "sub-a",
			rates:        []any{map[string]any{"limit": int64(10), "window": "1m"}},
			modelScoped:  "models-as-a-service/sub-a@llm/m1",
			groupKey:     "10/1m",
		},
	})
	if len(subNames) != 1 {
		t.Fatalf("subNames = %v, want 1", subNames)
	}
	if len(limits) != 1 {
		t.Fatalf("limits keys = %v, want 1 rate group", getKeys(limits))
	}
	for name, raw := range limits {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s: not a map", name)
		}
		when, ok := entry["when"].([]any)
		if !ok || len(when) == 0 {
			t.Fatalf("%s: missing when", name)
		}
		wm, ok := when[0].(map[string]any)
		if !ok {
			t.Fatalf("%s: when[0] not a map", name)
		}
		pred, _ := wm["predicate"].(string)
		if !strings.Contains(pred, `/v1/rerank`) {
			t.Errorf("predicate %q must scope to /v1/rerank", pred)
		}
	}
}

func TestBuildRequestCountGroupedLimits_unlimited(t *testing.T) {
	limits, _ := buildRequestCountGroupedLimits([]subInfo{
		{
			subNamespace: "models-as-a-service",
			subName:      "sub-unl",
			unlimited:    true,
			modelScoped:  "models-as-a-service/sub-unl@llm/m1",
		},
	})
	entry, ok := limits[unlimitedRequestLimitName].(map[string]any)
	if !ok {
		t.Fatalf("missing %q limit", unlimitedRequestLimitName)
	}
	if _, hasRates := entry["rates"]; hasRates {
		t.Errorf("unlimited request limit must not set rates")
	}
}
