package maas

import (
	"encoding/json"
	"testing"

	authorinocel "github.com/kuadrant/authorino/pkg/expressions/cel"
	authorinojson "github.com/kuadrant/authorino/pkg/json"
)

func TestGatewayCacheKeysUseAuthorinoExpressionResolver(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	cacheKeys := []struct {
		name string
		path []string
	}{
		{
			name: "apiKeyValidation",
			path: []string{"spec", "defaults", "rules", "metadata", "apiKeyValidation", "cache", "key"},
		},
		{
			name: "subscription-info",
			path: []string{"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key"},
		},
	}

	authorizationJSON := gatewayCacheKeyAuthorizationJSON(t, gatewayCacheKeyInput{
		apiKey:       "test-api-key-1", // #nosec G101 -- synthetic test data, not a credential.
		keyID:        "key-id-1",
		username:     "user-1",
		groups:       []any{"group-1"},
		subscription: "subscription-1",
		model:        "namespace-1/model-1",
	})
	changedAuthorizationJSON := gatewayCacheKeyAuthorizationJSON(t, gatewayCacheKeyInput{
		apiKey:       "test-api-key-2", // #nosec G101 -- synthetic test data, not a credential.
		keyID:        "key-id-2",
		username:     "user-2",
		groups:       []any{"group-2"},
		subscription: "subscription-2",
		model:        "namespace-1/model-2",
	})
	for _, cacheKey := range cacheKeys {
		t.Run(cacheKey.name, func(t *testing.T) {
			key := nestedMapRequired(t, obj, cacheKey.path...)
			if _, found := key["selector"]; found {
				t.Fatalf("cache key must not use the field-path selector field: %#v", key)
			}
			expression, ok := key["expression"].(string)
			if !ok || expression == "" {
				t.Fatalf("cache key must have a non-empty expression, got %#v", key)
			}

			selectorBaseline, err := (&authorinojson.JSONValue{Pattern: expression}).ResolveFor(authorizationJSON)
			if err != nil {
				t.Fatalf("resolve expression as Authorino selector: %v", err)
			}
			selectorChanged, err := (&authorinojson.JSONValue{Pattern: expression}).ResolveFor(changedAuthorizationJSON)
			if err != nil {
				t.Fatalf("resolve changed expression as Authorino selector: %v", err)
			}
			if selectorBaseline != selectorChanged {
				t.Fatalf("expression must not be emitted as a field-path selector; resolved to %#v and %#v", selectorBaseline, selectorChanged)
			}

			expressionValue, err := authorinocel.NewExpression(expression)
			if err != nil {
				t.Fatalf("compile Authorino CEL expression: %v", err)
			}
			resolvedKey, err := expressionValue.ResolveFor(authorizationJSON)
			if err != nil {
				t.Fatalf("resolve Authorino CEL expression: %v", err)
			}
			resolvedString, ok := resolvedKey.(string)
			if !ok || resolvedString == "" {
				t.Fatalf("Authorino CEL cache key must be a non-empty string, got %#v", resolvedKey)
			}
		})
	}
}

func TestGatewayCacheKeysIsolateAuthorinoInputs(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	baselineInput := gatewayCacheKeyInput{
		apiKey:       "test-api-key-1", // #nosec G101 -- synthetic test data, not a credential.
		keyID:        "key-id-1",
		username:     "user-1",
		groups:       []any{"group-1"},
		subscription: "subscription-1",
		model:        "namespace-1/model-1",
	}
	baselineJSON := gatewayCacheKeyAuthorizationJSON(t, baselineInput)
	variants := map[string]gatewayCacheKeyInput{
		"API key": {
			apiKey:       "test-api-key-2", // #nosec G101 -- synthetic test data, not a credential.
			keyID:        "key-id-2",
			username:     baselineInput.username,
			groups:       baselineInput.groups,
			subscription: baselineInput.subscription,
			model:        baselineInput.model,
		},
		"key ID": func() gatewayCacheKeyInput {
			variant := baselineInput
			variant.keyID = "key-id-2"
			return variant
		}(),
		"username": func() gatewayCacheKeyInput {
			variant := baselineInput
			variant.username = "user-2"
			return variant
		}(),
		"groups": func() gatewayCacheKeyInput {
			variant := baselineInput
			variant.groups = []any{"group-2"}
			return variant
		}(),
		"subscription": func() gatewayCacheKeyInput {
			variant := baselineInput
			variant.subscription = "subscription-2"
			return variant
		}(),
		"model": func() gatewayCacheKeyInput {
			variant := baselineInput
			variant.model = "namespace-1/model-2"
			return variant
		}(),
	}
	apiKeyOnlyVariants := map[string]gatewayCacheKeyInput{
		"API key": variants["API key"],
	}

	testCases := []struct {
		name     string
		path     []string
		variants map[string]gatewayCacheKeyInput
	}{
		{
			name:     "apiKeyValidation",
			path:     []string{"spec", "defaults", "rules", "metadata", "apiKeyValidation", "cache", "key", "expression"},
			variants: apiKeyOnlyVariants,
		},
		{
			name:     "subscription-info",
			path:     []string{"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key", "expression"},
			variants: variants,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			expression := nestedStringRequired(t, obj, testCase.path...)
			resolver, err := authorinocel.NewExpression(expression)
			if err != nil {
				t.Fatalf("compile Authorino CEL expression: %v", err)
			}
			baselineKey, err := resolver.ResolveFor(baselineJSON)
			if err != nil {
				t.Fatalf("resolve baseline cache key: %v", err)
			}

			for name, input := range testCase.variants {
				variantKey, err := resolver.ResolveFor(gatewayCacheKeyAuthorizationJSON(t, input))
				if err != nil {
					t.Fatalf("resolve %s cache key: %v", name, err)
				}
				if variantKey == baselineKey {
					t.Fatalf("cache key does not isolate %s; both resolved to %#v", name, baselineKey)
				}
			}
		})
	}
}

func TestGatewaySubscriptionCacheKeysIsolateAuthorizationIdentities(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	expressions := []struct {
		name string
		path []string
	}{
		{
			name: "subscription-info",
			path: []string{"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key", "expression"},
		},
	}

	oidcIdentity := func(username, subject, organizationID string, groups []any) map[string]any {
		return map[string]any{
			"iss":                "https://issuer.example.com",
			"sub":                subject,
			"preferred_username": username,
			"org_id":             organizationID,
			"groups":             groups,
		}
	}
	tokenReviewIdentity := func(username string, groups []any) map[string]any {
		return map[string]any{
			"user": map[string]any{
				"username": username,
				"groups":   groups,
			},
		}
	}

	fixtures := []struct {
		name              string
		authorizationJSON string
	}{
		{
			name: "API key",
			authorizationJSON: gatewayCacheKeyAuthorizationJSON(t, gatewayCacheKeyInput{
				apiKey:       "test-api-key-1", // #nosec G101 -- synthetic test data, not a credential.
				keyID:        "key-id-1",
				username:     "user-1",
				groups:       []any{"group-1"},
				subscription: "subscription-1",
				model:        "namespace-1/model-1",
			}),
		},
		{
			name: "OIDC username A",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				oidcIdentity("oidc-user-a", "oidc-subject-a", "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", []any{"oidc-group-a"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "OIDC username B",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				oidcIdentity("oidc-user-b", "oidc-subject-a", "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", []any{"oidc-group-a"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "OIDC organization B",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				oidcIdentity("oidc-user-a", "oidc-subject-a", "1e0ce6d3-3812-4dab-a090-1c8eacdddb4e", []any{"oidc-group-a"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "OIDC group B",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				oidcIdentity("oidc-user-a", "oidc-subject-a", "42c3e46f-2477-44d7-a85b-d3b43f6b31fa", []any{"oidc-group-b"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "TokenReview username A",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				tokenReviewIdentity("token-user-a", []any{"token-group-a"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "TokenReview username B",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				tokenReviewIdentity("token-user-b", []any{"token-group-a"}),
				"subscription-1", "namespace-1/model-1"),
		},
		{
			name: "TokenReview group B",
			authorizationJSON: gatewayIdentityCacheKeyAuthorizationJSON(t,
				tokenReviewIdentity("token-user-a", []any{"token-group-b"}),
				"subscription-1", "namespace-1/model-1"),
		},
	}

	for _, expression := range expressions {
		t.Run(expression.name, func(t *testing.T) {
			resolver, err := authorinocel.NewExpression(nestedStringRequired(t, obj, expression.path...))
			if err != nil {
				t.Fatalf("compile Authorino CEL expression: %v", err)
			}

			keys := make(map[string]string, len(fixtures))
			for _, fixture := range fixtures {
				resolvedKey, err := resolver.ResolveFor(fixture.authorizationJSON)
				if err != nil {
					t.Fatalf("resolve %s cache key: %v", fixture.name, err)
				}
				key, ok := resolvedKey.(string)
				if !ok || key == "" {
					t.Fatalf("%s cache key must be a non-empty string, got %#v", fixture.name, resolvedKey)
				}
				if existingFixture, found := keys[key]; found {
					t.Fatalf("%s and %s share cache key %q", existingFixture, fixture.name, key)
				}
				keys[key] = fixture.name
			}
		})
	}
}

type gatewayCacheKeyInput struct {
	apiKey       string
	keyID        string
	username     string
	groups       []any
	subscription string
	model        string
}

func gatewayCacheKeyAuthorizationJSON(t *testing.T, input gatewayCacheKeyInput) string {
	t.Helper()
	authorizationJSON := map[string]any{
		"request": map[string]any{
			"path": "/v1/chat/completions",
			"headers": map[string]any{
				"authorization":        "Bearer " + input.apiKey,
				"x-maas-subscription":  input.subscription,
				"x-gateway-model-name": input.model,
			},
		},
		"auth": map[string]any{
			"identity": "Bearer " + input.apiKey,
			"metadata": map[string]any{
				"apiKeyValidation": map[string]any{
					"userId":       input.keyID,
					"keyId":        input.keyID,
					"username":     input.username,
					"groups":       input.groups,
					"subscription": input.subscription,
				},
			},
		},
	}
	return gatewayAuthorizationJSON(t, authorizationJSON)
}

func gatewayIdentityCacheKeyAuthorizationJSON(t *testing.T, identity map[string]any, subscription, model string) string {
	t.Helper()
	return gatewayAuthorizationJSON(t, map[string]any{
		"request": map[string]any{
			"path": "/v1/chat/completions",
			"headers": map[string]any{
				"x-maas-subscription":  subscription,
				"x-gateway-model-name": model,
			},
		},
		"auth": map[string]any{
			"identity": identity,
		},
	})
}

func gatewayAuthorizationJSON(t *testing.T, authorizationJSON map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(authorizationJSON)
	if err != nil {
		t.Fatalf("marshal authorization JSON: %v", err)
	}
	return string(encoded)
}
