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

//nolint:testpackage
package maas

import (
	"encoding/json"
	"testing"

	"github.com/google/cel-go/cel"
)

func TestAPIKeyDenialCEL(t *testing.T) {
	tests := []struct {
		name       string
		xAPIKey     bool
		headers    map[string]any
		metadata   map[string]any
		wantMarker string
	}{
		{"expired key", false, map[string]any{"authorization": "Bearer sk-oai-expired"}, map[string]any{"apiKeyValidation": map[string]any{"valid": false, "reason": "expired"}}, "invalid"},
		{"revoked key", false, map[string]any{"authorization": "Bearer sk-oai-revoked"}, map[string]any{"apiKeyValidation": map[string]any{"valid": false, "reason": "revoked"}}, "invalid"},
		{"validation outage", false, map[string]any{"authorization": "Bearer sk-oai-valid"}, map[string]any{}, "unavailable"},
		{"malformed validation result", false, map[string]any{"authorization": "Bearer sk-oai-valid"}, map[string]any{"apiKeyValidation": map[string]any{}}, "unavailable"},
		{"permission denial", false, map[string]any{"authorization": "Bearer sk-oai-valid"}, map[string]any{"apiKeyValidation": map[string]any{"valid": true}}, ""},
		{"missing authorization", false, map[string]any{}, map[string]any{}, ""},
		{"non-key bearer", false, map[string]any{"authorization": "Bearer ordinary"}, map[string]any{}, ""},
		{"x-api-key expired", true, map[string]any{"x-api-key": "sk-oai-expired"}, map[string]any{"apiKeyValidation": map[string]any{"valid": false}}, "invalid"},
		{"x-api-key outage", true, map[string]any{"x-api-key": "sk-oai-valid"}, map[string]any{}, "unavailable"},
		{"x-api-key permission denial", true, map[string]any{"x-api-key": "sk-oai-valid"}, map[string]any{"apiKeyValidation": map[string]any{"valid": true}}, ""},
		{"x-api-key disabled", false, map[string]any{"x-api-key": "sk-oai-valid"}, map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &MaaSAuthPolicyReconciler{InfraNamespace: "maas-system"}
			spec := r.buildGatewayAuthPolicySpec(nil, tt.xAPIKey, "", "models-as-a-service", "gateway-ns", "gateway", nil)
			rules := spec["defaults"].(map[string]any)["rules"].(map[string]any)
			response := rules["response"].(map[string]any)
			unauthorized := response["unauthorized"].(map[string]any)
			headers := unauthorized["headers"].(map[string]any)
			expression := headers["x-maas-api-key-denial"].(map[string]any)["expression"].(string)
			env, err := cel.NewEnv(cel.Variable("auth", cel.DynType), cel.Variable("request", cel.DynType))
			if err != nil {
				t.Fatal(err)
			}
			ast, issues := env.Compile(expression)
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}
			program, err := env.Program(ast)
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := program.Eval(map[string]any{
				"auth":    map[string]any{"metadata": tt.metadata},
				"request": map[string]any{"headers": tt.headers},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := value.Value(); got != tt.wantMarker {
				t.Errorf("marker = %v, want %q", got, tt.wantMarker)
			}
		})
	}
}

func TestUnauthorizedResponseWithoutSubscriptionMetadata(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{InfraNamespace: "maas-system"}
	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "gateway-ns", "gateway", nil)
	rules := spec["defaults"].(map[string]any)["rules"].(map[string]any)
	unauthorized := rules["response"].(map[string]any)["unauthorized"].(map[string]any)
	bodyExpr := unauthorized["body"].(map[string]any)["expression"].(string)
	reasonExpr := unauthorized["headers"].(map[string]any)["x-ext-auth-reason"].(map[string]any)["expression"].(string)
	auth := map[string]any{"metadata": map[string]any{"apiKeyValidation": map[string]any{"valid": false}}}
	body := evaluateGatewayCELString(t, bodyExpr, auth)
	var decoded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unauthorized body is not JSON: %v", err)
	}
	if decoded.Error.Message != "Access denied" || decoded.Error.Type != "authorization_error" || decoded.Error.Code != 403 {
		t.Errorf("unexpected fallback denial body: %s", body)
	}
	if reason := evaluateGatewayCELString(t, reasonExpr, auth); reason != "unauthorized" {
		t.Errorf("x-ext-auth-reason = %q, want unauthorized", reason)
	}
}
