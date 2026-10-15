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
	"testing"

	"github.com/google/cel-go/cel"
)

// TestSubscriptionInfoFieldExpr evaluates the flow-control header expression the way
// Authorino does, with auth as a dynamic map. It must never fail: a failing response header
// drops the other headers in its priority group.
func TestSubscriptionInfoFieldExpr(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType))
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	ast, iss := env.Compile(subscriptionInfoFieldExpr("objective"))
	if iss.Err() != nil {
		t.Fatalf("Compile: %v", iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program: %v", err)
	}

	tests := []struct {
		name string
		auth map[string]any
		want string
	}{
		{name: "field set", auth: map[string]any{"metadata": map[string]any{
			"subscription-info": map[string]any{"objective": "maas-acme-gold-pool-0123456789"}}},
			want: "maas-acme-gold-pool-0123456789"},
		{name: "field absent", auth: map[string]any{"metadata": map[string]any{
			"subscription-info": map[string]any{"name": "gold"}}}},
		{name: "subscription-info error response", auth: map[string]any{"metadata": map[string]any{
			"subscription-info": map[string]any{"error": "not_found"}}}},
		{name: "subscription-info not fetched", auth: map[string]any{"metadata": map[string]any{
			"apiKeyValidation": map[string]any{"valid": true}}}},
		{name: "no metadata", auth: map[string]any{"identity": map[string]any{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := prg.Eval(map[string]any{"auth": tc.auth})
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got, ok := out.Value().(string); !ok || got != tc.want {
				t.Errorf("value = %#v, want %q", out.Value(), tc.want)
			}
		})
	}
}
