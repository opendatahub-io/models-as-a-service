/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func loadOracleLimits(t *testing.T) map[string]LimitadorLimit {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-limitador-site-a.json"))
	if err != nil {
		t.Fatalf("read oracle limits: %v", err)
	}
	var limits []LimitadorLimit
	if err := json.Unmarshal(data, &limits); err != nil {
		t.Fatalf("parse oracle limits: %v", err)
	}
	byName := map[string]LimitadorLimit{}
	for _, l := range limits {
		byName[l.Name] = l
	}
	return byName
}

func TestMergeLimitsPreservesOtherTenants(t *testing.T) {
	siteBToken := LimitadorLimit{Name: "site-b-tokens", Namespace: "ai-tenant-site-b/qwen3-kserve-route", MaxValue: 500}
	existing := []LimitadorLimit{
		{Name: "old-site-a-tokens", Namespace: "ai-tenant-site-a/qwen3-kserve-route", MaxValue: 100},
		{Name: "old-site-a-deny", Namespace: "redhat-ai-gateway-infra/maas-api-route-site-a", MaxValue: 0},
		siteBToken,
	}
	owned := map[string]bool{
		"ai-tenant-site-a/qwen3-kserve-route":           true,
		"redhat-ai-gateway-infra/maas-api-route-site-a": true,
	}
	ours := []LimitadorLimit{
		{Name: "ai-tenant-site-a-site-a-tier-qwen3-tokens", Namespace: "ai-tenant-site-a/qwen3-kserve-route", MaxValue: 200000},
		{Name: "deny-all-by-default", Namespace: "redhat-ai-gateway-infra/maas-api-route-site-a", MaxValue: 0},
	}

	got := mergeLimits(existing, owned, ours)

	foundB := false
	for _, l := range got {
		if reflect.DeepEqual(l, siteBToken) {
			foundB = true
		}
		if l.Name == "old-site-a-tokens" || l.Name == "old-site-a-deny" {
			t.Errorf("stale site-a limit %q survived the merge", l.Name)
		}
	}
	if !foundB {
		t.Errorf("site-b limit was clobbered")
	}
	if len(got) != 3 {
		t.Errorf("merged limits = %d, want 3 (site-b + 2 new site-a)", len(got))
	}
	if !slices.IsSortedFunc(got, func(a, b LimitadorLimit) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	}) {
		t.Errorf("merged limits not sorted: %v", got)
	}
}

func TestWindowSeconds(t *testing.T) {
	valid := map[string]int64{"1h": 3600, "1m": 60, "30s": 30, "24h": 86400, "1s": 1}
	for w, want := range valid {
		t.Run(w, func(t *testing.T) {
			got, err := windowSeconds(w)
			if err != nil || got != want {
				t.Errorf("windowSeconds(%q) = %d, %v; want %d", w, got, err, want)
			}
		})
	}
	for _, bad := range []string{"", "h", "10x", "1d", "abc"} {
		t.Run("invalid/"+bad, func(t *testing.T) {
			if _, err := windowSeconds(bad); !errors.Is(err, ErrInvalidWindow) {
				t.Errorf("windowSeconds(%q) error = %v; want ErrInvalidWindow", bad, err)
			}
		})
	}
}

func TestBuildLimitadorLimitMatchesOracle(t *testing.T) {
	oracle := loadOracleLimits(t)

	tok, err := buildLimitadorLimit(
		"ai-tenant-site-a-site-a-tier-qwen3-tokens",
		"ai-tenant-site-a/qwen3-kserve-route",
		"ai_tenant_site_a_site_a_tier_qwen3_tokens__b42c1f29",
		200000, "1h")
	if err != nil {
		t.Fatalf("build token limit: %v", err)
	}
	if want := oracle["ai-tenant-site-a-site-a-tier-qwen3-tokens"]; !reflect.DeepEqual(tok, want) {
		gj := mustJSON(t, tok)
		wj := mustJSON(t, want)
		t.Errorf("token limit mismatch:\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}

	deny, err := buildLimitadorLimit(
		"deny-all-by-default",
		"redhat-ai-gateway-infra/maas-api-route-site-a",
		"deny_all_by_default__d3136ae7",
		0, "1m")
	if err != nil {
		t.Fatalf("build deny-all limit: %v", err)
	}
	if want := oracle["deny-all-by-default"]; !reflect.DeepEqual(deny, want) {
		gj := mustJSON(t, deny)
		wj := mustJSON(t, want)
		t.Errorf("deny-all limit mismatch:\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}
}
