/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	envoytype "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// The cases run each request through newAuthorinoGateway and assert what other
// components get: the calls maas-api receives, the headers sent upstream, the identity
// TokenRateLimitPolicy reads and the deny response. Every failure the harness reports
// also fails the case.

const (
	celTestOrganizationID = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	celTestKeyID          = "8f0c2f9e-5d1a-4b7e-9c3d-2a6b1e4f7c90"
	celTestModelPath      = "/llm/model-a/v1/chat/completions"
	celTestInfraNamespace = "maas-system"
)

// celTestAPIKeyValidation is maas-api's validate response for a valid key. groups is
// omitted when empty, as maas-api's omitempty does.
func celTestAPIKeyValidation(groups ...string) map[string]any {
	validation := map[string]any{
		"valid":        true,
		"userId":       celTestKeyID,
		"username":     "alice",
		"keyId":        celTestKeyID,
		"keyName":      "ci",
		"subscription": "gold",
		"tenant":       "",
		"groupsJSON":   mustJSON(append([]string{}, groups...)),
	}
	if len(groups) > 0 {
		validation["groups"] = groups
	}
	return validation
}

var celTestInvalidAPIKey = map[string]any{"valid": false, "tenant": "", "reason": "key not found"}

// celTestSubscriptionInfo is maas-api's select response for a subscription without
// guardrail checks.
func celTestSubscriptionInfo() map[string]any {
	return map[string]any{
		"name":                    "gold",
		"namespace":               "models-as-a-service",
		"phase":                   "Active",
		"accessAllowed":           true,
		"resolvedModel":           "llm/model-a",
		"rateLimitId":             "a1b2c3",
		"objective":               "gold-objective",
		"fairnessId":              "models-as-a-service",
		"organizationId":          celTestOrganizationID,
		"costCenter":              "cc-42",
		"selectedGuardrailChecks": map[string]any{},
		"headers":                 map[string]any{"x-grid-team": []any{"blue"}},
	}
}

func celTestOIDCClaims() map[string]any {
	return map[string]any{
		"iss":                "https://issuer.example.com/realms/test",
		"sub":                "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
		"aud":                []any{"maas-api", "account"},
		"azp":                "maas-client",
		"token_use":          "access",
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"groups":             []any{"users"},
		"org_id":             celTestOrganizationID,
	}
}

type celPolicyVariant struct {
	oidc             *oidcConfig
	xAPIKeyEnabled   bool
	tenantID         string
	guardrails       []guardrailHeader
	subscriptionKeys []string
}

// celPolicyVariants are the policy shapes the reconciler generates. Gateway policies
// always get a guardrail catalog, possibly empty; nil would drop the guardrail rule.
func celPolicyVariants() map[string]celPolicyVariant {
	noGuardrails := []guardrailHeader{}
	return map[string]celPolicyVariant{
		"default":   {guardrails: noGuardrails},
		"x-api-key": {xAPIKeyEnabled: true, guardrails: noGuardrails},
		"oidc-audience": {guardrails: noGuardrails, oidc: &oidcConfig{
			IssuerURL:               "https://issuer.example.com/realms/test",
			ClientID:                "maas-client",
			Audience:                "maas-api",
			AccessTokenPurposeClaim: "token_use",
			AccessTokenPurpose:      "access",
			TTL:                     300,
		}},
		"oidc-no-audience": {guardrails: noGuardrails, oidc: &oidcConfig{
			IssuerURL: "https://issuer.example.com/realms/test",
			ClientID:  "maas-client",
			TTL:       300,
		}},
		"tenant-guardrails": {
			tenantID:         "tenant-abc",
			guardrails:       []guardrailHeader{{ID: "pii", Name: "x-aigateway-guardrail-pii"}},
			subscriptionKeys: []string{"x-grid-team"},
		},
	}
}

func (v celPolicyVariant) spec(t *testing.T) map[string]any {
	t.Helper()
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   celTestInfraNamespace,
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "gateway-ns",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
		AuthzCacheTTL:    60,
	}
	spec := r.buildGatewayAuthPolicySpec(v.oidc, v.xAPIKeyEnabled, v.tenantID, "models-as-a-service", "gateway-ns", "maas-default-gateway", v.guardrails)
	if err := applySubscriptionHeaders(spec, v.subscriptionKeys); err != nil {
		t.Fatalf("applySubscriptionHeaders: %v", err)
	}
	return spec
}

type gatewayPolicyCase struct {
	name     string
	variants []string
	request  gatewayRequest
	maasAPI  maasAPIReplies
	// wantAllowed is Authorino's verdict.
	wantAllowed bool
	// wantMaaSAPICalls lists the maas-api endpoints the gateway calls, in order.
	wantMaaSAPICalls []string
	// wantSelectBody is the body sent to subscription selection. Nil skips the check.
	wantSelectBody map[string]any
	// wantHeaders are upstream headers and their values, by lower-case name.
	wantHeaders map[string]string
	// wantNoHeaders name upstream headers that must not be sent.
	wantNoHeaders []string
	// wantIdentityClaims are claims the X-MaaS-Identity envelope must carry. maas-api
	// rejects an envelope without sub or aud. Nil skips the check.
	wantIdentityClaims map[string]any
	// wantRateLimitIdentity says whether TokenRateLimitPolicy gets auth.identity, and
	// wantRateLimitIdentityFields are fields it must carry. Nil skips the fields.
	wantRateLimitIdentity       bool
	wantRateLimitIdentityFields map[string]any
	wantDenyBody                string
	wantReason                  string
	// wantDeniedBy is the authorization rule that denies the request. Set it only when
	// that rule alone denies; with several, which one Authorino reports is chance.
	wantDeniedBy string
}

func gatewayPolicyCases() []gatewayPolicyCase {
	everyVariant := []string{"default", "x-api-key", "oidc-audience", "oidc-no-audience", "tenant-guardrails"}
	oidcVariants := []string{"oidc-audience", "oidc-no-audience"}
	accessDenied := `{"error":{"message":"Access denied","type":"authorization_error","code":403}}`
	validateAndSelect := []string{validateEndpoint, selectEndpoint}
	guardrailSubscription := celTestSubscriptionInfo()
	guardrailSubscription["selectedGuardrailChecks"] = map[string]any{"pii": true}
	// bodyRoutedModel is the body model maas-api reports for an ExternalModel path model.
	bodyRoutedSubscription := celTestSubscriptionInfo()
	bodyRoutedSubscription["bodyRoutedModel"] = "gpt-4o"
	bodyModelRequest := func(model string) gatewayRequest {
		request := apiKeyRequest(celTestModelPath)
		request.headers["x-gateway-model-name"] = model
		return request
	}
	// A Kubernetes service account token is a JWT. The OIDC method rejects it, and
	// TokenReview authenticates it at the next priority.
	serviceAccountJWTRequest := tokenReviewRequest(celTestModelPath, []string{"system:authenticated", "dev"})
	serviceAccountJWTRequest.headers["authorization"] = "Bearer aaa.bbb.ccc"
	// Each of these breaks one rule while every other rule allows the request, so
	// that rule alone denies it.
	subscription := func(change func(map[string]any)) map[string]any {
		info := celTestSubscriptionInfo()
		change(info)
		return info
	}
	validationExpiring := func(expiresAt string) map[string]any {
		validation := celTestAPIKeyValidation("system:authenticated")
		validation["expiresAt"] = expiresAt
		return validation
	}
	oidcClaims := func(change func(map[string]any)) map[string]any {
		claims := celTestOIDCClaims()
		change(claims)
		return claims
	}
	clientIdentityHeaderRequest := apiKeyRequest(celTestModelPath)
	clientIdentityHeaderRequest.headers["x-maas-username"] = "mallory"
	bodyRoutedRequest := apiKeyRequest("/v1/chat/completions")
	bodyRoutedRequest.headers["x-gateway-model-name"] = "llm/model-b"
	validKey := celTestAPIKeyValidation("system:authenticated")

	return []gatewayPolicyCase{
		{
			name:     "valid API key on a model route",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated", "dev"),
				selectEndpoint:   celTestSubscriptionInfo(),
			},
			wantAllowed:      true,
			wantMaaSAPICalls: validateAndSelect,
			wantHeaders: map[string]string{
				"x-maas-username":                   "alice",
				"x-maas-group":                      `["system:authenticated","dev"]`,
				"x-maas-subscription":               "gold",
				"x-maas-subscription-rate-limit-id": "a1b2c3",
				"x-maas-cost-center":                "cc-42",
				"x-maas-organization-id":            celTestOrganizationID,
				"x-maas-keyname":                    "ci",
			},
			wantRateLimitIdentity: true,
			wantRateLimitIdentityFields: map[string]any{
				"userid":                    "alice",
				"groups":                    []any{"system:authenticated", "dev"},
				"groups_str":                "system:authenticated,dev",
				"keyId":                     celTestKeyID,
				"keyName":                   "ci",
				"selected_subscription":     "gold",
				"selected_subscription_id":  "a1b2c3",
				"selected_subscription_key": "models-as-a-service/gold@llm/model-a",
			},
		},
		{
			name:     "valid API key without groups selects a subscription",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation(),
				selectEndpoint:   celTestSubscriptionInfo(),
			},
			wantAllowed:      true,
			wantMaaSAPICalls: validateAndSelect,
			// maas-api matches grants on these groups, so the fallback must not add any.
			wantSelectBody: map[string]any{
				"groups":                []any{},
				"username":              "alice",
				"requestedSubscription": "gold",
				"requestedModel":        "llm/model-a",
			},
			wantHeaders:           map[string]string{"x-maas-username": "alice", "x-maas-group": "[]"},
			wantRateLimitIdentity: true,
		},
		{
			name:     "selected guardrail check reaches the guardrail header",
			variants: []string{"tenant-guardrails"},
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   guardrailSubscription,
			},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantHeaders:           map[string]string{"x-aigateway-guardrail-pii": "true", "x-grid-team": "blue"},
			wantRateLimitIdentity: true,
		},
		{
			name:     "guardrail check outside the catalog is denied",
			variants: []string{"default"},
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   guardrailSubscription,
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "invalid API key is denied without subscription selection",
			variants:         everyVariant,
			request:          apiKeyRequest(celTestModelPath),
			maasAPI:          maasAPIReplies{validateEndpoint: celTestInvalidAPIKey},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "API key whose validation failed is denied without subscription selection",
			variants:         everyVariant,
			request:          apiKeyRequest(celTestModelPath),
			maasAPI:          maasAPIReplies{validateEndpoint: maasAPIUnreachable},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "API key with a validation error response is denied without subscription selection",
			variants:         everyVariant,
			request:          apiKeyRequest(celTestModelPath),
			maasAPI:          maasAPIReplies{validateEndpoint: map[string]any{"error": "service unavailable"}},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:     "valid API key whose subscription selection failed is denied",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   maasAPIUnreachable,
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:     "maas-api selection error reaches the deny body",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint: map[string]any{
					"error":   "access_denied",
					"message": "user \"alice\" has no access\nto llm/model-a",
				},
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     `{"error":{"message":"user \"alice\" has no access\nto llm/model-a","type":"authorization_error","code":403}}`,
			wantReason:       "access_denied",
		},
		{
			name:     "body model matching the path model is allowed",
			variants: everyVariant,
			request:  bodyModelRequest("gpt-4o"),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   bodyRoutedSubscription,
			},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantHeaders:           map[string]string{"x-maas-subscription": "gold"},
			wantRateLimitIdentity: true,
		},
		{
			name:     "body model other than the path model is denied as a mismatch",
			variants: everyVariant,
			request:  bodyModelRequest("gpt-4o-mini"),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   bodyRoutedSubscription,
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     `{"error":{"message":"Access denied: model in the request body does not match model in the request path","type":"authorization_error","code":403}}`,
			wantReason:       "model_mismatch",
		},
		{
			name:     "body model on a request whose subscription selection failed is denied",
			variants: everyVariant,
			request:  bodyModelRequest("gpt-4o-mini"),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   maasAPIUnreachable,
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "valid API key on a maas-api route",
			variants:         everyVariant,
			request:          apiKeyRequest("/maas-api/v1/models"),
			maasAPI:          maasAPIReplies{validateEndpoint: celTestAPIKeyValidation("system:authenticated")},
			wantAllowed:      true,
			wantMaaSAPICalls: []string{validateEndpoint},
			wantHeaders: map[string]string{
				"x-maas-username":     "alice",
				"x-maas-group":        `["system:authenticated"]`,
				"x-maas-subscription": "gold",
			},
			wantNoHeaders: []string{"x-maas-subscription-rate-limit-id", "x-maas-cost-center"},
		},
		{
			name:     "x-api-key on a model route",
			variants: []string{"x-api-key"},
			request:  xAPIKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
				selectEndpoint:   celTestSubscriptionInfo(),
			},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantHeaders:           map[string]string{"x-maas-username": "alice", "x-maas-group": `["system:authenticated"]`},
			wantRateLimitIdentity: true,
		},
		{
			name:             "invalid x-api-key is denied without subscription selection",
			variants:         []string{"x-api-key"},
			request:          xAPIKeyRequest(celTestModelPath),
			maasAPI:          maasAPIReplies{validateEndpoint: celTestInvalidAPIKey},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "TokenReview identity on a model route",
			variants:         everyVariant,
			request:          tokenReviewRequest(celTestModelPath, []string{"system:authenticated", "dev"}),
			maasAPI:          maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantAllowed:      true,
			wantMaaSAPICalls: []string{selectEndpoint},
			wantSelectBody: map[string]any{
				"groups":                []any{"system:authenticated", "dev"},
				"username":              "system:serviceaccount:ns:sa",
				"requestedSubscription": "gold",
				"requestedModel":        "llm/model-a",
			},
			wantHeaders:           map[string]string{"x-maas-username": "system:serviceaccount:ns:sa"},
			wantRateLimitIdentity: true,
			wantRateLimitIdentityFields: map[string]any{
				"userid":                "system:serviceaccount:ns:sa",
				"groups":                []any{"system:authenticated", "dev"},
				"keyId":                 "",
				"selected_subscription": "gold",
			},
		},
		{
			name:                  "service account JWT on an OIDC gateway is a TokenReview identity",
			variants:              oidcVariants,
			request:               serviceAccountJWTRequest,
			maasAPI:               maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantAllowed:           true,
			wantMaaSAPICalls:      []string{selectEndpoint},
			wantHeaders:           map[string]string{"x-maas-username": "system:serviceaccount:ns:sa"},
			wantRateLimitIdentity: true,
		},
		{
			name:        "TokenReview identity without groups on a maas-api route",
			variants:    everyVariant,
			request:     tokenReviewRequest("/maas-api/v1/subscriptions", nil),
			wantAllowed: true,
			wantHeaders: map[string]string{
				"x-maas-username": "system:serviceaccount:ns:sa",
				"x-maas-group":    `["system:authenticated"]`,
			},
		},
		{
			name:             "TokenReview identity whose subscription selection failed is denied",
			variants:         everyVariant,
			request:          tokenReviewRequest(celTestModelPath, []string{"system:authenticated"}),
			maasAPI:          maasAPIReplies{selectEndpoint: maasAPIUnreachable},
			wantMaaSAPICalls: []string{selectEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
		},
		{
			name:             "OIDC identity on a model route",
			variants:         oidcVariants,
			request:          oidcRequest(celTestModelPath, celTestOIDCClaims()),
			maasAPI:          maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantAllowed:      true,
			wantMaaSAPICalls: []string{selectEndpoint},
			// Groups of an organization-scoped token are qualified with the organization.
			wantSelectBody: map[string]any{
				"groups":                []any{celTestOrganizationID + "/users"},
				"username":              "alice",
				"requestedSubscription": "",
				"requestedModel":        "llm/model-a",
			},
			wantHeaders: map[string]string{
				"x-maas-username":        "alice",
				"x-maas-organization-id": celTestOrganizationID,
			},
			// Organization-scoped tokens carry no group header.
			wantNoHeaders:         []string{"x-maas-group"},
			wantRateLimitIdentity: true,
			wantRateLimitIdentityFields: map[string]any{
				"userid": "alice",
				"groups": []any{celTestOrganizationID + "/users"},
			},
		},
		{
			name:     "OIDC identity with a single string aud on a model route",
			variants: oidcVariants,
			request: oidcRequest(celTestModelPath, map[string]any{
				"iss":       "https://issuer.example.com/realms/test",
				"sub":       "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
				"aud":       "maas-api",
				"azp":       "maas-client",
				"token_use": "access",
				"groups":    []any{"users"},
			}),
			maasAPI:          maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantAllowed:      true,
			wantMaaSAPICalls: []string{selectEndpoint},
			wantHeaders: map[string]string{
				"x-maas-username": "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
				"x-maas-group":    `["system:authenticated","users"]`,
			},
			wantRateLimitIdentity: true,
		},
		{
			name:        "OIDC identity on /v1/me gets the verified identity header",
			variants:    oidcVariants,
			request:     oidcRequest("/v1/me", celTestOIDCClaims()),
			wantAllowed: true,
			wantHeaders: map[string]string{"x-maas-username": "alice"},
			wantIdentityClaims: map[string]any{
				"sub": "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
				"aud": []any{"maas-api", "account"},
			},
		},
		{
			// maas-api rejects an identity envelope without aud or sub but accepts a
			// request without the header, so the header is skipped and the request
			// keeps the username and group headers.
			name:     "OIDC identity without aud on /v1/me keeps the legacy identity headers",
			variants: []string{"oidc-no-audience"},
			request: oidcRequest("/v1/me", map[string]any{
				"iss": "https://issuer.example.com/realms/test",
				"sub": "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
				"azp": "maas-client",
			}),
			wantAllowed: true,
			wantHeaders: map[string]string{
				"x-maas-username": "7d1f6a52-0b0c-4f43-9b52-4e3c7a0f8d11",
				"x-maas-group":    `["system:authenticated"]`,
			},
			wantNoHeaders: []string{"x-maas-identity"},
		},
		{
			name:             "invalid API key on a maas-api route is denied",
			variants:         everyVariant,
			request:          apiKeyRequest("/maas-api/v1/models"),
			maasAPI:          maasAPIReplies{validateEndpoint: celTestInvalidAPIKey},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "auth-valid",
		},
		{
			name:             "expired API key is denied",
			variants:         everyVariant,
			request:          apiKeyRequest(celTestModelPath),
			maasAPI:          maasAPIReplies{validateEndpoint: validationExpiring("2020-01-01T00:00:00Z"), selectEndpoint: celTestSubscriptionInfo()},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "auth-valid",
		},
		{
			// Rego parses expiry to nanoseconds, so a date past 2262 doesn't parse.
			name:                  "API key that expires later is allowed",
			variants:              everyVariant,
			request:               apiKeyRequest(celTestModelPath),
			maasAPI:               maasAPIReplies{validateEndpoint: validationExpiring("2099-01-01T00:00:00Z"), selectEndpoint: celTestSubscriptionInfo()},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantRateLimitIdentity: true,
		},
		{
			name:     "pending subscription is denied",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: validKey,
				selectEndpoint:   subscription(func(info map[string]any) { info["phase"] = "Pending" }),
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "subscription-valid",
		},
		{
			name:     "degraded subscription is allowed",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: validKey,
				selectEndpoint:   subscription(func(info map[string]any) { info["phase"] = "Degraded" }),
			},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantRateLimitIdentity: true,
		},
		{
			name:     "subscription being deleted is denied",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: validKey,
				selectEndpoint:   subscription(func(info map[string]any) { info["deletionTimestamp"] = "2026-11-26T10:00:00Z" }),
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "subscription-valid",
		},
		{
			name:     "subscription without a name is denied",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: validKey,
				selectEndpoint:   subscription(func(info map[string]any) { info["name"] = "" }),
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "subscription-valid",
		},
		{
			name:     "subscription that doesn't grant access is denied",
			variants: everyVariant,
			request:  apiKeyRequest(celTestModelPath),
			maasAPI: maasAPIReplies{
				validateEndpoint: validKey,
				selectEndpoint:   subscription(func(info map[string]any) { info["accessAllowed"] = false }),
			},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "require-group-membership",
		},
		{
			name:             "API key on the key-management API is denied",
			variants:         everyVariant,
			request:          apiKeyRequest("/maas-api/v1/api-keys"),
			maasAPI:          maasAPIReplies{validateEndpoint: validKey},
			wantMaaSAPICalls: []string{validateEndpoint},
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "deny-api-key-management",
		},
		{
			name:             "client-supplied identity header is denied",
			variants:         everyVariant,
			request:          clientIdentityHeaderRequest,
			maasAPI:          maasAPIReplies{validateEndpoint: validKey, selectEndpoint: celTestSubscriptionInfo()},
			wantMaaSAPICalls: validateAndSelect,
			wantDenyBody:     accessDenied,
			wantReason:       "unauthorized",
			wantDeniedBy:     "deny-client-identity-headers",
		},
		{
			name:     "API key on a body-routed endpoint selects the header model",
			variants: everyVariant,
			request:  bodyRoutedRequest,
			maasAPI:  maasAPIReplies{validateEndpoint: validKey, selectEndpoint: celTestSubscriptionInfo()},
			wantSelectBody: map[string]any{
				"groups":                []any{"system:authenticated"},
				"username":              "alice",
				"requestedSubscription": "gold",
				"requestedModel":        "llm/model-b",
			},
			wantAllowed:           true,
			wantMaaSAPICalls:      validateAndSelect,
			wantRateLimitIdentity: true,
		},
		{
			name:         "OIDC token for another client is denied",
			variants:     oidcVariants,
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["azp"] = "other-client" })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-client-bound",
		},
		{
			name:         "OIDC token from another issuer is denied",
			variants:     oidcVariants,
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["iss"] = "https://other.example.com" })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-client-bound",
		},
		{
			name:         "OIDC token for other audiences is denied",
			variants:     []string{"oidc-audience"},
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["aud"] = []any{"other"} })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-client-bound",
		},
		{
			name:         "OIDC token with a single other audience is denied",
			variants:     []string{"oidc-audience"},
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["aud"] = "other" })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-client-bound",
		},
		{
			name:         "OIDC token of another purpose is denied",
			variants:     []string{"oidc-audience"},
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["token_use"] = "id" })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-client-bound",
		},
		{
			// An organization-scoped token's groups are qualified by the gateway, so a
			// group that already looks qualified could claim another organization's.
			name:         "OIDC token with an already-qualified group is denied",
			variants:     oidcVariants,
			request:      oidcRequest("/v1/me", oidcClaims(func(claims map[string]any) { claims["groups"] = []any{celTestOrganizationID + "/users"} })),
			wantDenyBody: accessDenied,
			wantReason:   "unauthorized",
			wantDeniedBy: "oidc-groups-safe",
		},
	}
}

func TestGatewayAuthPolicyOnAuthorino(t *testing.T) {
	variants := celPolicyVariants()
	for _, tc := range gatewayPolicyCases() {
		for _, variantName := range tc.variants {
			variant, ok := variants[variantName]
			if !ok {
				t.Fatalf("case %q names unknown variant %q", tc.name, variantName)
			}
			t.Run(tc.name+"/"+variantName, func(t *testing.T) {
				// given
				gateway := newAuthorinoGateway(t, variant.spec(t), tc.maasAPI)

				// when
				outcome := gateway.evaluate(t, tc.request)

				// then
				if got := outcome.calledEndpoints(); !slices.Equal(got, tc.wantMaaSAPICalls) {
					t.Errorf("maas-api calls = %v, want %v", got, tc.wantMaaSAPICalls)
				}
				// The gateway calls the TLS port of the maas-api Service the tenant reconcile
				// creates in the infrastructure namespace.
				wantOrigin := "https://" + tenantreconcile.MaaSAPIServiceName(variant.tenantID) + "." + celTestInfraNamespace + ".svc.cluster.local:8443"
				for _, call := range outcome.maasAPICalls {
					if origin := call.scheme + "://" + call.host; origin != wantOrigin {
						t.Errorf("maas-api %s called on %s, want %s", call.endpoint, origin, wantOrigin)
					}
				}
				if outcome.authenticatedBy != tc.request.authMethod {
					t.Errorf("authenticated by %q, want %q", outcome.authenticatedBy, tc.request.authMethod)
				}
				// API-key requests carry sk-oai-key; maas-api validates it without the
				// Bearer prefix.
				if slices.Contains(outcome.calledEndpoints(), validateEndpoint) {
					if got := outcome.callBody(validateEndpoint); !reflect.DeepEqual(got, map[string]any{"key": "sk-oai-key"}) {
						t.Errorf("API key validation body = %#v, want the bare key", got)
					}
				}
				if got := outcome.callBody(selectEndpoint); tc.wantSelectBody != nil && !reflect.DeepEqual(got, tc.wantSelectBody) {
					t.Errorf("subscription selection body = %#v, want %#v", got, tc.wantSelectBody)
				}
				if outcome.allowed != tc.wantAllowed {
					t.Fatalf("allowed = %v (status %v, body %q), want %v", outcome.allowed, outcome.status, outcome.body, tc.wantAllowed)
				}
				if !tc.wantAllowed {
					assertDenyResponse(t, outcome, tc.wantDenyBody, tc.wantReason)
					if tc.wantDeniedBy != "" && outcome.deniedBy != tc.wantDeniedBy {
						t.Errorf("denied by %q, want %q", outcome.deniedBy, tc.wantDeniedBy)
					}
					return
				}
				for name, want := range tc.wantHeaders {
					if got, ok := outcome.headers[name]; !ok || got != want {
						t.Errorf("upstream header %s = %q (sent: %v), want %q", name, got, ok, want)
					}
				}
				for _, name := range tc.wantNoHeaders {
					if got, ok := outcome.headers[name]; ok {
						t.Errorf("upstream header %s = %q, want it skipped", name, got)
					}
				}
				if tc.wantIdentityClaims != nil {
					assertIdentityClaims(t, outcome.headers["x-maas-identity"], tc.wantIdentityClaims)
				}
				identity, got := outcome.metadata["identity"]
				if got != tc.wantRateLimitIdentity {
					t.Errorf("rate-limit identity set = %v, want %v", got, tc.wantRateLimitIdentity)
				}
				if tc.wantRateLimitIdentityFields != nil {
					fields, _ := identity.(map[string]any)
					for field, want := range tc.wantRateLimitIdentityFields {
						if got := fields[field]; !reflect.DeepEqual(got, want) {
							t.Errorf("rate-limit identity %s = %#v, want %#v", field, got, want)
						}
					}
				}
			})
		}
	}
}

func assertIdentityClaims(t *testing.T, header string, want map[string]any) {
	t.Helper()
	var envelope struct {
		Claims map[string]any `json:"claims"`
	}
	if err := json.Unmarshal([]byte(header), &envelope); err != nil {
		t.Fatalf("X-MaaS-Identity %q is not JSON: %v", header, err)
	}
	for claim, value := range want {
		if got := envelope.Claims[claim]; !reflect.DeepEqual(got, value) {
			t.Errorf("X-MaaS-Identity claim %s = %#v, want %#v", claim, got, value)
		}
	}
}

func assertDenyResponse(t *testing.T, outcome gatewayOutcome, wantBody, wantReason string) {
	t.Helper()
	if outcome.status != envoytype.StatusCode_Forbidden {
		t.Errorf("deny status = %v, want Forbidden", outcome.status)
	}
	var got, want any
	if err := json.Unmarshal([]byte(outcome.body), &got); err != nil {
		t.Fatalf("deny body %q is not JSON: %v", outcome.body, err)
	}
	if err := json.Unmarshal([]byte(wantBody), &want); err != nil {
		t.Fatalf("want body %q is not JSON: %v", wantBody, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deny body = %s, want %s", outcome.body, wantBody)
	}
	if reason := outcome.headers["x-ext-auth-reason"]; reason != wantReason {
		t.Errorf("x-ext-auth-reason = %q, want %q", reason, wantReason)
	}
}

// Every policy variant compiles on RHCL's Authorino. newAuthorinoGateway runs the same
// check; this one fails fast, without starting Authorino.
func TestGatewayAuthPolicyCELCompilesForRHCL(t *testing.T) {
	for name, variant := range celPolicyVariants() {
		t.Run(name, func(t *testing.T) {
			assertAllAuthPolicyCEL_RHCLCompatible(t, variant.spec(t))
		})
	}
}

// A request without an Authorization header is unauthenticated, and no authentication
// `when` fails on the missing header.
func TestGatewayAuthPolicyWithoutAuthorizationHeader(t *testing.T) {
	variants := celPolicyVariants()
	oidcVariant, ok := variants["oidc-audience"]
	if !ok {
		t.Fatal("celPolicyVariants has no oidc-audience variant")
	}
	// With x-api-key and OIDC both on, the OIDC `when` reads the header past the
	// API-key check.
	variants["oidc-x-api-key"] = celPolicyVariant{
		oidc:           oidcVariant.oidc,
		xAPIKeyEnabled: true,
		guardrails:     []guardrailHeader{},
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			// given
			gateway := newAuthorinoGateway(t, variant.spec(t), nil)

			// when
			outcome := gateway.evaluate(t, gatewayRequest{path: celTestModelPath})

			// then
			if outcome.allowed || outcome.status != envoytype.StatusCode_Unauthorized || outcome.authenticatedBy != "" {
				t.Errorf("allowed = %v, status = %v, authenticated by %q, want Unauthorized", outcome.allowed, outcome.status, outcome.authenticatedBy)
			}
		})
	}
}
