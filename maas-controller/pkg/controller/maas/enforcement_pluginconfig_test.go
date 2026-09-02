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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// loadOraclePluginConfig loads captured Kuadrant pluginConfig output, used as the
// diff oracle for generation.
func loadOraclePluginConfig(t *testing.T) PluginConfiguration {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-pluginconfig-site-a.json"))
	if err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	var pc PluginConfiguration
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("parse oracle: %v", err)
	}
	return pc
}

// loadOracleActionSet loads the reference Kuadrant action set for a
// /v1/chat/completions model route; the generator must reproduce it (except the
// internal name hash).
func loadOracleActionSet(t *testing.T) ActionSet {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-actionset-chat-completions.json"))
	if err != nil {
		t.Fatalf("read oracle action set: %v", err)
	}
	var a ActionSet
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("parse oracle action set: %v", err)
	}
	return a
}

func TestRoutePredicates(t *testing.T) {
	cases := []struct {
		name  string
		match RouteMatch
		want  []string
	}{
		{
			name: "exact with header",
			match: RouteMatch{
				PathType:    "Exact",
				PathValue:   "/v1/chat/completions",
				HeaderName:  "X-Gateway-Model-Name",
				HeaderValue: "publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B",
			},
			want: []string{
				"request.url_path == '/v1/chat/completions'",
				"request.headers.exists(h, h.lowerAscii() == 'x-gateway-model-name' && request.headers[h] == 'publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B')",
			},
		},
		{
			name:  "path prefix",
			match: RouteMatch{PathType: "PathPrefix", PathValue: "/ai-tenant-site-a/qwen3"},
			want:  []string{"request.url_path.startsWith('/ai-tenant-site-a/qwen3')"},
		},
		{
			name:  "exact without header",
			match: RouteMatch{PathType: "Exact", PathValue: "/v1/models"},
			want:  []string{"request.url_path == '/v1/models'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routePredicates(tc.match); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("routePredicates mismatch:\n got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

// qwen3RouteMatches is a model route's flattened match list, compiling to the
// model action sets in the oracle.
func qwen3RouteMatches() []RouteMatch {
	const modelHdr = "publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B"
	hdr := func(pathType, path string) RouteMatch {
		return RouteMatch{PathType: pathType, PathValue: path, HeaderName: "X-Gateway-Model-Name", HeaderValue: modelHdr}
	}
	pfx := func(path string) RouteMatch { return RouteMatch{PathType: "PathPrefix", PathValue: path} }
	var m []RouteMatch
	for _, p := range []string{
		"/v1/completions", "/v1/completions/", "/v1/chat/completions", "/v1/chat/completions/",
		"/v1/responses", "/v1/responses/", "/v1/messages", "/v1/messages/",
	} {
		m = append(m, hdr("Exact", p))
	}
	for _, p := range []string{
		"/ai-tenant-site-a/qwen3/v1/completions", "/ai-tenant-site-a/qwen3/v1/chat/completions",
		"/ai-tenant-site-a/qwen3/v1/responses", "/ai-tenant-site-a/qwen3/v1/messages",
	} {
		m = append(m, pfx(p))
	}
	for _, p := range []string{
		"/publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B/v1/completions",
		"/publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B/v1/chat/completions",
		"/publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B/v1/responses",
		"/publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B/v1/messages",
	} {
		m = append(m, pfx(p))
	}
	m = append(m, pfx("/publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B"))
	m = append(m, pfx("/ai-tenant-site-a/qwen3"))
	m = append(m, hdr("PathPrefix", "/"))
	return m
}

func TestBuildRouteActionSetsAggregatesModelRoute(t *testing.T) {
	base := ModelActionSetParams{
		Hostname:        "site-a.example.com",
		AuthScope:       "gateway-authconfig-host", // our single AuthConfig; Kuadrant uses per-rule hashes
		AuthSource:      "authpolicy.kuadrant.io:openshift-ingress/maas-site-a-gateway-maas-auth",
		RLScope:         "ai-tenant-site-a/qwen3-kserve-route",
		TRLPSource:      "tokenratelimitpolicy.kuadrant.io:ai-tenant-site-a/maas-trlp-qwen3",
		SubscriptionKey: "ai-tenant-site-a/site-a-tier@ai-tenant-site-a/qwen3",
		TokenLimitKey:   "ai_tenant_site_a_site_a_tier_qwen3_tokens__b42c1f29",
	}
	got := buildRouteActionSets(qwen3RouteMatches(), base)

	// Collect the oracle's model action sets (ratelimit scope is the model route).
	oracle := loadOraclePluginConfig(t)
	wantConds := map[string]bool{}
	for _, a := range oracle.ActionSets {
		if a.Actions[1].Scope == "ai-tenant-site-a/qwen3-kserve-route" {
			wantConds[actionSetCondKey(a)] = true
		}
	}
	if len(got) != len(wantConds) {
		t.Fatalf("generated %d model action sets, oracle has %d", len(got), len(wantConds))
	}
	for _, a := range got {
		if !wantConds[actionSetCondKey(a)] {
			t.Errorf("generated an action set with no oracle match: %v", a.RouteRuleConditions.Predicates)
		}
		if a.Actions[1].Scope != base.RLScope {
			t.Errorf("ratelimit-check scope = %q, want %q", a.Actions[1].Scope, base.RLScope)
		}
		if a.Actions[2].ConditionalData[0].Data[0].Expression.Key != "tokenlimit."+base.TokenLimitKey {
			t.Errorf("report token key = %q", a.Actions[2].ConditionalData[0].Data[0].Expression.Key)
		}
	}
}

func TestGenerateGatewayArtifactsMultiSubscription(t *testing.T) {
	in := GatewayEnforcementInput{
		Hostname:        "site-a.example.com",
		AuthConfigHost:  "gw-authconfig-host",
		AuthSource:      "authpolicy.kuadrant.io:openshift-ingress/maas-site-a-gateway-maas-auth",
		AuthRules:       map[string]any{"authentication": map[string]any{}},
		ModelMatches:    []RouteMatch{{PathType: "Exact", PathValue: "/v1/chat/completions"}},
		ModelRouteScope: "ai-tenant-site-a/qwen3-kserve-route",
		ModelSource:     "tokenratelimitpolicy.kuadrant.io:ai-tenant-site-a/maas-trlp-qwen3",
		Subscriptions: []SubscriptionBinding{
			{SubscriptionKey: "ai-tenant-site-a/alice@ai-tenant-site-a/qwen3", TokenLimitKey: "alice__k", LimitName: "alice-qwen3-tokens", MaxValue: 100, Window: "1h"},
			{SubscriptionKey: "ai-tenant-site-a/bob@ai-tenant-site-a/qwen3", TokenLimitKey: "bob__k", LimitName: "bob-qwen3-tokens", MaxValue: 200, Window: "1h"},
		},
		MaasAPIMatches: []RouteMatch{{PathType: "PathPrefix", PathValue: "/maas-api"}},
		DenyAll:        SubscriptionBinding{RouteScope: "redhat-ai-gateway-infra/maas-api-route-site-a", Source: "trlp:deny", TokenLimitKey: "deny__k", LimitName: "deny-all-by-default", MaxValue: 0, Window: "1m"},
	}

	arts, err := generateGatewayArtifacts(in)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// One Limitador limit per subscription, plus the deny-all.
	if len(arts.Limits) != 3 {
		t.Errorf("limits = %d, want 3 (alice, bob, deny-all)", len(arts.Limits))
	}

	// Each model action set's ratelimit-check and -report carry two descriptors.
	modelSets := 0
	for _, a := range arts.PluginConfig.ActionSets {
		if a.Actions[1].Scope != in.ModelRouteScope {
			continue
		}
		modelSets++
		for _, actIdx := range []int{1, 2} { // check, report
			if got := len(a.Actions[actIdx].ConditionalData); got != 2 {
				t.Errorf("action %d conditionalData = %d, want 2 (alice+bob)", actIdx, got)
			}
		}
	}
	if modelSets == 0 {
		t.Errorf("no model action sets generated")
	}
}

// TestGenerateGatewayArtifacts is a composition test: one gathered
// input produces all three artifacts, each matching its oracle.
func TestGenerateGatewayArtifacts(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "redhat-ai-gateway-infra",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
	}
	authSpec := r.buildGatewayAuthPolicySpec(nil, false, "site-a", "site-a", "openshift-ingress", "maas-site-a-gateway")
	authRules, ok := authPolicyRules(authSpec)
	if !ok {
		t.Fatalf("extract auth rules")
	}

	in := GatewayEnforcementInput{
		Hostname:        "site-a.example.com",
		AuthConfigHost:  "gateway-authconfig-host",
		AuthSource:      "authpolicy.kuadrant.io:openshift-ingress/maas-site-a-gateway-maas-auth",
		AuthRules:       authRules,
		ModelMatches:    qwen3RouteMatches(),
		ModelRouteScope: "ai-tenant-site-a/qwen3-kserve-route",
		ModelSource:     "tokenratelimitpolicy.kuadrant.io:ai-tenant-site-a/maas-trlp-qwen3",
		Subscriptions: []SubscriptionBinding{{
			SubscriptionKey: "ai-tenant-site-a/site-a-tier@ai-tenant-site-a/qwen3",
			TokenLimitKey:   "ai_tenant_site_a_site_a_tier_qwen3_tokens__b42c1f29",
			LimitName:       "ai-tenant-site-a-site-a-tier-qwen3-tokens",
			MaxValue:        200000,
			Window:          "1h",
		}},
		MaasAPIMatches: maasAPIRouteMatches(),
		DenyAll: SubscriptionBinding{ //nolint:gosec // descriptor key is a Kuadrant identifier, not a credential
			RouteScope:    "redhat-ai-gateway-infra/maas-api-route-site-a",
			Source:        "tokenratelimitpolicy.kuadrant.io:openshift-ingress/gateway-default-deny-site-a",
			TokenLimitKey: "deny_all_by_default__d3136ae7",
			LimitName:     "deny-all-by-default",
			MaxValue:      0,
			Window:        "1m",
		},
	}

	arts, err := generateGatewayArtifacts(in)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// pluginConfig: all 23 action sets match on route conditions and RL binding.
	oraclePC := loadOraclePluginConfig(t)
	want := map[string]string{}
	for _, a := range oraclePC.ActionSets {
		want[actionSetCondKey(a)] = actionSetRLKey(a)
	}
	got := map[string]string{}
	for _, a := range arts.PluginConfig.ActionSets {
		got[actionSetCondKey(a)] = actionSetRLKey(a)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pluginConfig action sets differ from oracle (got %d, want %d)", len(got), len(want))
	}

	// AuthConfig: host set, authentication matches oracle.
	if hosts, _ := arts.AuthConfig["hosts"].([]any); len(hosts) != 1 || hosts[0] != in.AuthConfigHost {
		t.Errorf("authConfig hosts = %v", arts.AuthConfig["hosts"])
	}
	oracleAC := loadOracleAuthConfig(t)
	if gA, wA := jsonNormalize(t, arts.AuthConfig["authentication"]), jsonNormalize(t, oracleAC["authentication"]); !reflect.DeepEqual(gA, wA) {
		t.Errorf("authConfig authentication differs from oracle")
	}

	// Limits: both match the oracle.
	oracleLim := loadOracleLimits(t)
	if len(arts.Limits) != 2 {
		t.Fatalf("got %d limits, want 2", len(arts.Limits))
	}
	for _, l := range arts.Limits {
		if want, ok := oracleLim[l.Name]; !ok || !reflect.DeepEqual(l, want) {
			t.Errorf("limit %q differs from oracle", l.Name)
		}
	}
}

// maasAPIRouteMatches is a maas-api route: four control-plane path prefixes, no
// header.
func maasAPIRouteMatches() []RouteMatch {
	pfx := func(p string) RouteMatch { return RouteMatch{PathType: "PathPrefix", PathValue: p} }
	return []RouteMatch{pfx("/v1/models"), pfx("/v1/subscriptions"), pfx("/v1/api-keys"), pfx("/maas-api")}
}

func TestAssembleGatewayPluginConfigMatchesOracle(t *testing.T) {
	pc := assembleGatewayPluginConfig(GatewayPluginParams{
		Hostname:     "site-a.example.com",
		AuthScope:    "gateway-authconfig-host",
		AuthSource:   "authpolicy.kuadrant.io:openshift-ingress/maas-site-a-gateway-maas-auth",
		ModelMatches: qwen3RouteMatches(),
		ModelRL: RateLimitBinding{
			Scope:  "ai-tenant-site-a/qwen3-kserve-route",
			Source: "tokenratelimitpolicy.kuadrant.io:ai-tenant-site-a/maas-trlp-qwen3",
			Descriptors: []RateLimitDescriptor{{
				TokenLimitKey: "ai_tenant_site_a_site_a_tier_qwen3_tokens__b42c1f29",
				WhenPredicate: modelWhenPredicate("ai-tenant-site-a/site-a-tier@ai-tenant-site-a/qwen3"),
			}},
		},
		MaasAPIMatches: maasAPIRouteMatches(),
		DenyAllRL: RateLimitBinding{
			Scope:  "redhat-ai-gateway-infra/maas-api-route-site-a",
			Source: "tokenratelimitpolicy.kuadrant.io:openshift-ingress/gateway-default-deny-site-a",
			Descriptors: []RateLimitDescriptor{{ //nolint:gosec // descriptor key is a Kuadrant identifier, not a credential
				TokenLimitKey: "deny_all_by_default__d3136ae7",
				WhenPredicate: denyAllWhenPredicate,
			}},
		},
	})

	oracle := loadOraclePluginConfig(t)

	if len(pc.ActionSets) != len(oracle.ActionSets) {
		t.Fatalf("generated %d action sets, oracle has %d", len(pc.ActionSets), len(oracle.ActionSets))
	}

	// Diff every action set's route conditions and ratelimit binding against the
	// oracle. Auth scope is intentionally not compared (we use one AuthConfig
	// per gateway; Kuadrant uses per-rule hashes).
	want := map[string]string{}
	for _, a := range oracle.ActionSets {
		want[actionSetCondKey(a)] = actionSetRLKey(a)
	}
	got := map[string]string{}
	for _, a := range pc.ActionSets {
		got[actionSetCondKey(a)] = actionSetRLKey(a)
	}
	if !reflect.DeepEqual(got, want) {
		for k, wv := range want {
			if gv, ok := got[k]; !ok {
				t.Errorf("missing action set: %s", k)
			} else if gv != wv {
				t.Errorf("rl binding mismatch for %s:\n got %s\nwant %s", k, gv, wv)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("unexpected action set: %s", k)
			}
		}
	}

	// The services block must match the oracle (structurally).
	if !reflect.DeepEqual(pc.Services, oracle.Services) {
		gj := mustJSON(t, pc.Services)
		wj := mustJSON(t, oracle.Services)
		t.Errorf("services mismatch:\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}
}

func TestBuildModelActionSetMatchesOracle(t *testing.T) {
	want := loadOracleActionSet(t)

	got := buildModelActionSet(ModelActionSetParams{
		Hostname:        "site-a.example.com",
		RoutePredicates: routePredicates(RouteMatch{PathType: "Exact", PathValue: "/v1/chat/completions", HeaderName: "X-Gateway-Model-Name", HeaderValue: "publishers/ai-tenant-site-a/models/Qwen/Qwen3-0.6B"}),
		AuthScope:       "410f8ee4ef0bb20b515d4c56db735b69d29d244f2091bfcb8bd32f5c93ef312f",
		AuthSource:      "authpolicy.kuadrant.io:openshift-ingress/maas-site-a-gateway-maas-auth",
		RLScope:         "ai-tenant-site-a/qwen3-kserve-route",
		TRLPSource:      "tokenratelimitpolicy.kuadrant.io:ai-tenant-site-a/maas-trlp-qwen3",
		SubscriptionKey: "ai-tenant-site-a/site-a-tier@ai-tenant-site-a/qwen3",
		TokenLimitKey:   "ai_tenant_site_a_site_a_tier_qwen3_tokens__b42c1f29",
	})

	// The internal name hash is ours, not Kuadrant's, so compare only conditions
	// and actions.
	if !reflect.DeepEqual(got.RouteRuleConditions, want.RouteRuleConditions) {
		t.Errorf("routeRuleConditions mismatch:\n got %#v\nwant %#v", got.RouteRuleConditions, want.RouteRuleConditions)
	}
	if !reflect.DeepEqual(got.Actions, want.Actions) {
		gj := mustJSON(t, got.Actions)
		wj := mustJSON(t, want.Actions)
		t.Errorf("actions mismatch:\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}
}
