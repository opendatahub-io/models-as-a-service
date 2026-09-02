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
	"fmt"
	"testing"
)

// These benchmark the control-plane compile path (run per config-change reconcile,
// not per request). They exist to catch super-linear cost as tenants grow.

func benchInput(nSubs, nMatches int) GatewayEnforcementInput {
	in := GatewayEnforcementInput{
		Hostname:        "gw.example.com",
		AuthConfigHost:  "authhost",
		AuthSource:      "authpolicy.kuadrant.io:ns/gw-maas-auth",
		ModelRouteScope: "ns/model-route",
		ModelSource:     "tokenratelimitpolicy.kuadrant.io:ns/model-route",
		MaasAPIMatches:  []RouteMatch{{PathType: "PathPrefix", PathValue: "/maas-api"}},
		DenyAll: SubscriptionBinding{
			RouteScope:    "ns/maas-api-route",
			Source:        "tokenratelimitpolicy.kuadrant.io:ns/maas-api-route",
			TokenLimitKey: descriptorKey("deny-all-by-default"),
			LimitName:     "deny-all-by-default",
			Window:        "1m",
		},
	}
	for i := range nMatches {
		in.ModelMatches = append(in.ModelMatches, RouteMatch{PathType: "Exact", PathValue: fmt.Sprintf("/v1/p%d", i)})
	}
	for i := range nSubs {
		name := fmt.Sprintf("ns-sub%d-model-tokens", i)
		in.Subscriptions = append(in.Subscriptions, SubscriptionBinding{
			SubscriptionKey: fmt.Sprintf("ns/sub%d@ns/model", i),
			TokenLimitKey:   descriptorKey(name),
			LimitName:       name,
			MaxValue:        int64(1000 * (i + 1)),
			Window:          "1h",
		})
	}
	return in
}

func BenchmarkGenerateGatewayArtifacts(b *testing.B) {
	in := benchInput(50, 20)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := generateGatewayArtifacts(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssembleGatewayPluginConfig(b *testing.B) {
	in := benchInput(50, 20)
	p := GatewayPluginParams{
		Hostname:       in.Hostname,
		AuthScope:      in.AuthConfigHost,
		AuthSource:     in.AuthSource,
		ModelMatches:   in.ModelMatches,
		ModelRL:        RateLimitBinding{Scope: in.ModelRouteScope, Source: in.ModelSource},
		MaasAPIMatches: in.MaasAPIMatches,
		DenyAllRL:      RateLimitBinding{Scope: in.DenyAll.RouteScope, Source: in.DenyAll.Source, Descriptors: []RateLimitDescriptor{in.DenyAll.descriptor(true)}},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = assembleGatewayPluginConfig(p)
	}
}

func BenchmarkMergeLimits(b *testing.B) {
	// A shared CR holding many tenants' limits; this gateway owns one scope.
	var existing []LimitadorLimit
	for i := range 500 {
		existing = append(existing, LimitadorLimit{Name: fmt.Sprintf("t%d", i), Namespace: fmt.Sprintf("ns%d/route", i)})
	}
	owned := map[string]bool{"ns0/route": true}
	ours := []LimitadorLimit{{Name: "new", Namespace: "ns0/route", MaxValue: 100}}
	b.ReportAllocs()
	for b.Loop() {
		_ = mergeLimits(existing, owned, ours)
	}
}
