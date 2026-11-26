/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const pathRoutedInference = "/llm/q-plain/v1/chat/completions"

func apiKeyAuthContext() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"apiKeyValidation": map[string]any{
			"valid":        true,
			"userId":       "key-user-id",
			"keyId":        "key-id",
			"username":     "alice",
			"subscription": "q-plain-sub",
			"groups":       []any{"team-a"},
		}},
		"identity": map[string]any{},
	}
}

// subscriptionInfoAuth adds info as the subscription-info metadata; nil leaves it out.
func subscriptionInfoAuth(info map[string]any) map[string]any {
	auth := apiKeyAuthContext()
	if metadata, ok := auth["metadata"].(map[string]any); ok && info != nil {
		metadata["subscription-info"] = info
	}
	return auth
}

// noHeader leaves X-Gateway-Model-Name out; "" sends it empty.
const noHeader = "\x00"

func inferenceRequest(path, bodyModel string) map[string]any {
	headers := map[string]any{"authorization": "Bearer sk-oai-test"}
	if bodyModel != noHeader {
		headers["x-gateway-model-name"] = bodyModel
	}
	return map[string]any{"path": path, "headers": headers}
}

// The body-model check adds nothing to the subscription-info request, which
// maas-api decodes with DisallowUnknownFields, so a new controller in front of
// an older maas-api keeps serving: the old API simply omits bodyRoutedModel and
// the rule passes.
func TestGatewayAuthPolicy_SubscriptionInfoRequestUnchanged(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	body := nestedStringRequired(t, obj, "spec", "defaults", "rules", "metadata", "subscription-info", "http", "body", "expression")
	got := evaluateGatewayCEL[map[string]any](t, body, apiKeyAuthContext(), inferenceRequest(pathRoutedInference, "gpt-4o-mini"))
	want := map[string]any{
		"groups":                []any{"team-a"},
		"username":              "alice",
		"requestedSubscription": "q-plain-sub",
		"requestedModel":        "llm/q-plain",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("subscription-info body = %#v, want %#v", got, want)
	}
}

func TestGatewayAuthPolicy_BodyRoutedModelRule(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	rule := []string{"spec", "defaults", "rules", "authorization", "body-routed-model-matches"}
	if _, cached := nestedMapRequired(t, obj, rule...)["cache"]; cached {
		t.Error("body-routed-model-matches must not be cached: the header is in no cache key")
	}
	when := nestedWhenPredicateRequired(t, obj, append(rule, "when")...)
	patterns, _, err := unstructured.NestedSlice(obj.Object, append(rule, "patternMatching", "patterns")...)
	if err != nil || len(patterns) != 1 {
		t.Fatalf("patterns = %#v (err %v), want one", patterns, err)
	}
	pattern, _ := patterns[0].(map[string]any)["predicate"].(string)

	field := func(value string) map[string]any { return map[string]any{"name": "sub", "bodyRoutedModel": value} }
	tests := []struct {
		name, path, header string
		info               map[string]any
		wantDenied         bool
	}{
		{"match", pathRoutedInference, "gpt-renamed", field("gpt-renamed"), false},
		{"other model", pathRoutedInference, "gpt-4o-mini", field("q-plain"), true},
		{"namespace/name form", pathRoutedInference, "llm/q-plain", field("q-plain"), true},
		{"case differs", pathRoutedInference, "Q-Plain", field("q-plain"), true},
		{"unknown alias is present but empty", pathRoutedInference, "q-plain", field(""), true},
		{"empty field and empty header", pathRoutedInference, "", field(""), true},
		{"empty header", pathRoutedInference, "", field("q-plain"), true},
		{"field absent (KServe or older maas-api)", pathRoutedInference, "x", map[string]any{"name": "sub"}, false},
		{"subscription-info absent", pathRoutedInference, "x", nil, false},
		{"no header", pathRoutedInference, noHeader, field("q-plain"), false},
		{"body-routed request", "/v1/chat/completions", "gpt-4o-mini", field("q-plain"), false},
		{"maas-api request", "/maas-api/v1/models", "gpt-4o-mini", field("q-plain"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, request := subscriptionInfoAuth(tt.info), inferenceRequest(tt.path, tt.header)
			// The pattern is evaluated only when `when` holds. The helper fails on an
			// evaluation error, which would make Authorino skip the rule.
			denied := evaluateGatewayCEL[bool](t, when, auth, request) && !evaluateGatewayCEL[bool](t, pattern, auth, request)
			if denied != tt.wantDenied {
				t.Errorf("denied = %v, want %v", denied, tt.wantDenied)
			}
		})
	}
}

// The 403 reports the mismatch unless maas-api sent a message of its own.
func TestGatewayAuthPolicy_BodyRoutedModelDenialResponse(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	unauthorized := []string{"spec", "defaults", "rules", "response", "unauthorized"}
	bodyExpression := nestedStringRequired(t, obj, append(unauthorized, "body", "expression")...)
	reasonExpression := nestedStringRequired(t, obj, append(unauthorized, "headers", "x-ext-auth-reason", "expression")...)

	allowed := map[string]any{"name": "sub", "accessAllowed": true, "bodyRoutedModel": "q-plain"}
	const denial = `Access denied: model "llm/q-plain" is not available in subscription "gold"`
	tests := []struct {
		name, path, header      string
		info                    map[string]any
		wantMessage, wantReason string
	}{
		{
			"mismatch", pathRoutedInference, "gpt-4o-mini", allowed,
			"Access denied: model in the request body does not match model in the request path", "model_mismatch",
		},
		{
			"maas-api message wins", pathRoutedInference, "gpt-4o-mini",
			map[string]any{"error": "access_denied", "message": denial, "bodyRoutedModel": "q-plain"},
			denial, "access_denied",
		},
		{"matching body model denied elsewhere", pathRoutedInference, "q-plain", allowed, "Access denied", "unauthorized"},
		{"body-routed request", "/v1/chat/completions", "gpt-4o-mini", allowed, "Access denied", "unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, request := subscriptionInfoAuth(tt.info), inferenceRequest(tt.path, tt.header)
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			rendered := evaluateGatewayCEL[string](t, bodyExpression, auth, request)
			if err := json.Unmarshal([]byte(rendered), &body); err != nil {
				t.Fatalf("403 body is not valid JSON: %v: %s", err, rendered)
			}
			if body.Error.Message != tt.wantMessage {
				t.Errorf("403 message = %q, want %q", body.Error.Message, tt.wantMessage)
			}
			if reason := evaluateGatewayCEL[string](t, reasonExpression, auth, request); reason != tt.wantReason {
				t.Errorf("x-ext-auth-reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
