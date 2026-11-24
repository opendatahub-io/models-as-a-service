package maas

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
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
			if !reflect.DeepEqual(selectorBaseline, selectorChanged) {
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
			cacheKeyString(t, resolvedKey, cacheKey.name)
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
	subscriptionInfoVariants := map[string]gatewayCacheKeyInput{
		"API key":      variants["API key"],
		"key ID":       variants["key ID"],
		"username":     variants["username"],
		"subscription": variants["subscription"],
		"model":        variants["model"],
	}
	// An API key's groups come from its stored row, so they never differ for the same
	// key ID, and the subscription-info key leaves them out.
	sameKeyIDVariants := map[string]gatewayCacheKeyInput{
		"groups": variants["groups"],
	}

	testCases := []struct {
		name     string
		path     []string
		variants map[string]gatewayCacheKeyInput
		sameKey  map[string]gatewayCacheKeyInput
	}{
		{
			name:     "apiKeyValidation",
			path:     []string{"spec", "defaults", "rules", "metadata", "apiKeyValidation", "cache", "key", "expression"},
			variants: apiKeyOnlyVariants,
		},
		{
			name:     "subscription-info",
			path:     []string{"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key", "expression"},
			variants: subscriptionInfoVariants,
			sameKey:  sameKeyIDVariants,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			expression := nestedStringRequired(t, obj, testCase.path...)
			resolver, err := authorinocel.NewExpression(expression)
			if err != nil {
				t.Fatalf("compile Authorino CEL expression: %v", err)
			}
			resolvedBaseline, err := resolver.ResolveFor(baselineJSON)
			if err != nil {
				t.Fatalf("resolve baseline cache key: %v", err)
			}
			baselineKey := cacheKeyString(t, resolvedBaseline, "baseline")

			for name, input := range testCase.variants {
				resolvedVariant, err := resolver.ResolveFor(gatewayCacheKeyAuthorizationJSON(t, input))
				if err != nil {
					t.Fatalf("resolve %s cache key: %v", name, err)
				}
				if variantKey := cacheKeyString(t, resolvedVariant, name); variantKey == baselineKey {
					t.Fatalf("cache key does not isolate %s; both resolved to %#v", name, baselineKey)
				}
			}
			for name, input := range testCase.sameKey {
				resolvedVariant, err := resolver.ResolveFor(gatewayCacheKeyAuthorizationJSON(t, input))
				if err != nil {
					t.Fatalf("resolve %s cache key: %v", name, err)
				}
				if variantKey := cacheKeyString(t, resolvedVariant, name); variantKey != baselineKey {
					t.Fatalf("cache key changes with %s for the same key ID: %#v and %#v", name, baselineKey, variantKey)
				}
			}
		})
	}
}

// TestSubscriptionInfoCacheKeyCoversRequestBody checks the property that makes the
// subscription-info cache safe: requests that share a cache key send the same request body,
// so a cached response is always the response maas-api would have returned.
func TestSubscriptionInfoCacheKeyCoversRequestBody(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	keyResolver := compileAuthorinoCEL(t, nestedStringRequired(t, obj,
		"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key", "expression"))
	bodyResolver := compileAuthorinoCEL(t, nestedStringRequired(t, obj,
		"spec", "defaults", "rules", "metadata", "subscription-info", "http", "body", "expression"))

	// API keys as maas-api returns them: the stored row is fixed per key ID.
	apiKeyRows := map[string]map[string]any{
		"key-id-1": {"username": "user-1", "groups": []any{"group-1"}, "subscription": "subscription-1"},
		"key-id-2": {"username": "user-1", "groups": []any{"group-1"}, "subscription": "subscription-1"},
		"key-id-3": {"username": "user-2", "groups": []any{"group-1", "group-2"}, "subscription": "subscription-2"},
		// Keys minted by OIDC identities with an org_id store org-qualified groups; the same
		// user and group name in two organizations must not share a cached selection.
		"key-id-org-a": {"username": "user-1", "groups": []any{
			"system:authenticated", "42c3e46f-2477-44d7-a85b-d3b43f6b31fa/team",
		}, "subscription": "subscription-1"},
		"key-id-org-b": {"username": "user-1", "groups": []any{
			"system:authenticated", "1e0ce6d3-3812-4dab-a090-1c8eacdddb4e/team",
		}, "subscription": "subscription-1"},
	}
	identities := map[string]map[string]any{
		"OIDC user-1": {"identity": map[string]any{
			"iss": "https://issuer.example.com", "sub": "subject-1", "preferred_username": "user-1", "groups": []any{"group-1"},
		}},
		"OIDC user-1 group-2": {"identity": map[string]any{
			"iss": "https://issuer.example.com", "sub": "subject-1", "preferred_username": "user-1", "groups": []any{"group-2"},
		}},
		"TokenReview user-1": {"identity": map[string]any{
			"user": map[string]any{"username": "user-1", "groups": []any{"group-1"}},
		}},
		"TokenReview user-1 group-2": {"identity": map[string]any{
			"user": map[string]any{"username": "user-1", "groups": []any{"group-2"}},
		}},
	}
	for keyID, row := range apiKeyRows {
		validation := map[string]any{"valid": true, "userId": keyID, "keyId": keyID}
		maps.Copy(validation, row)
		identities["API key "+keyID] = map[string]any{
			"identity": "Bearer test-api-key-" + keyID, // #nosec G101 -- synthetic test data, not a credential.
			"metadata": map[string]any{"apiKeyValidation": validation},
		}
	}
	// Responses without a usable key ID take the full key, which must still separate their groups.
	for name, keyID := range map[string]any{"without key ID": nil, "with empty key ID": ""} {
		for _, group := range []string{"group-1", "group-2"} {
			validation := map[string]any{
				"valid": true, "userId": "legacy-user", "username": "user-1", "groups": []any{group}, "subscription": "subscription-1",
			}
			if keyID != nil {
				validation["keyId"] = keyID
			}
			identities["API key "+name+" "+group] = map[string]any{
				"identity": "Bearer test-api-key-legacy", // #nosec G101 -- synthetic test data, not a credential.
				"metadata": map[string]any{"apiKeyValidation": validation},
			}
		}
	}
	type routeInput struct {
		path    string
		headers map[string]any
	}
	routes := map[string]routeInput{
		"header model-1": {path: "/v1/chat/completions", headers: map[string]any{"x-gateway-model-name": "namespace-1/model-1"}},
		"header model-2": {path: "/v1/chat/completions", headers: map[string]any{"x-gateway-model-name": "namespace-1/model-2"}},
		"path model-1":   {path: "/namespace-1/model-1/v1/chat/completions", headers: map[string]any{}},
	}
	subscriptionHeaders := []string{"", "subscription-1", "subscription-2"}

	type seen struct {
		request string
		body    any
	}
	byKey := map[string]seen{}
	sharedKeys := 0
	for identityName, auth := range identities {
		for routeName, route := range routes {
			for _, subscriptionHeader := range subscriptionHeaders {
				headers := maps.Clone(route.headers)
				if subscriptionHeader != "" {
					headers["x-maas-subscription"] = subscriptionHeader
				}
				request := fmt.Sprintf("%s, %s, x-maas-subscription=%q", identityName, routeName, subscriptionHeader)
				authorizationJSON := gatewayAuthorizationJSON(t, map[string]any{
					"request": map[string]any{"path": route.path, "headers": headers},
					"auth":    auth,
				})

				key, err := keyResolver.ResolveFor(authorizationJSON)
				if err != nil {
					t.Fatalf("resolve cache key for %s: %v", request, err)
				}
				keyString := cacheKeyString(t, key, request)
				body, err := bodyResolver.ResolveFor(authorizationJSON)
				if err != nil {
					t.Fatalf("resolve request body for %s: %v", request, err)
				}

				previous, found := byKey[keyString]
				if !found {
					byKey[keyString] = seen{request: request, body: body}
					continue
				}
				sharedKeys++
				if !reflect.DeepEqual(previous.body, body) {
					t.Fatalf("%s and %s share cache key %q but send different bodies:\n%#v\n%#v",
						previous.request, request, keyString, previous.body, body)
				}
			}
		}
	}
	if sharedKeys == 0 {
		t.Fatal("no two requests share a cache key, so the fixtures never exercise a cache hit")
	}
}

// TestSubscriptionInfoCacheKeyIgnoresSubscriptionHeaderForAPIKeys: an API key's subscription is
// bound when it is created, so a client-chosen x-maas-subscription value must not split its cache.
func TestSubscriptionInfoCacheKeyIgnoresSubscriptionHeaderForAPIKeys(t *testing.T) {
	obj := gatewayAuthPolicySpecTestObject(t, nil)
	resolver := compileAuthorinoCEL(t, nestedStringRequired(t, obj,
		"spec", "defaults", "rules", "metadata", "subscription-info", "cache", "key", "expression"))
	keys := map[string]string{}
	for _, header := range []string{"", "subscription-1", "subscription-2"} {
		headers := map[string]any{"x-gateway-model-name": "namespace-1/model-1"}
		if header != "" {
			headers["x-maas-subscription"] = header
		}
		key, err := resolver.ResolveFor(gatewayAuthorizationJSON(t, map[string]any{
			"request": map[string]any{"path": "/v1/chat/completions", "headers": headers},
			"auth": map[string]any{
				"identity": "Bearer test-api-key-1", // #nosec G101 -- synthetic test data, not a credential.
				"metadata": map[string]any{"apiKeyValidation": map[string]any{
					"valid": true, "userId": "key-id-1", "keyId": "key-id-1", "username": "user-1",
					"groups": []any{"group-1"}, "subscription": "subscription-1",
				}},
			},
		}))
		if err != nil {
			t.Fatalf("resolve cache key with x-maas-subscription=%q: %v", header, err)
		}
		keyString := cacheKeyString(t, key, fmt.Sprintf("x-maas-subscription=%q", header))
		keys[keyString] = header
	}
	if len(keys) != 1 {
		t.Fatalf("x-maas-subscription splits the API-key cache key: %v", keys)
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
				key := cacheKeyString(t, resolvedKey, fixture.name)
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

// cacheKeyString returns the key inside the one-element list a cache key expression
// resolves to. The list is what makes Authorino store a hash of the key instead of the key.
func cacheKeyString(t *testing.T, resolved any, context string) string {
	t.Helper()
	list, ok := resolved.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("%s: cache key must resolve to a one-element list, got %#v", context, resolved)
	}
	key, ok := list[0].(string)
	if !ok || key == "" {
		t.Fatalf("%s: cache key must wrap a non-empty string, got %#v", context, list[0])
	}
	return key
}

func compileAuthorinoCEL(t *testing.T, expression string) *authorinocel.Expression {
	t.Helper()
	resolver, err := authorinocel.NewExpression(expression)
	if err != nil {
		t.Fatalf("compile Authorino CEL expression: %v", err)
	}
	return resolver
}
