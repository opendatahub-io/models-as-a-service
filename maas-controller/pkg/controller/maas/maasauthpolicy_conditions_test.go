/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"maps"
	"slices"
	"strings"
	"testing"

	envoytype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// These tests run the generated gateway AuthPolicy on newAuthorinoGateway, one gateway per
// request, and pin what the guarded rules do to each request: whether it is denied, which
// maas-api endpoints it calls and which success headers reach upstream. They assert
// outcomes rather than expression text, so a condition can be rewritten without changing
// them.

const (
	conditionsFixtureOrgID      = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
	conditionsFixtureIssuer     = "https://issuer.example.com/realms/test"
	conditionsFixtureAPIKey     = "sk-oai-FAKEFAKEFAKEFAKEFAKEFAKE0123456789" //nolint:gosec // fake key fixture
	conditionsFixtureSubHeader  = "x-grid-team"
	conditionsFixtureModelPath  = "/llm/qwen3/v1/chat/completions"
	conditionsFixtureGuardrailA = "x-aigateway-guardrail-pii"
	conditionsFixtureGuardrailB = "x-aigateway-guardrail-toxicity"
	conditionsAccessDenied      = `{"error":{"message":"Access denied","type":"authorization_error","code":403}}`
)

type conditionsCredential struct {
	name string
	// authMethod is the authentication method that authenticates the credential.
	authMethod       string
	apiKey           bool
	xAPIKey          bool // sent as x-api-key, only authenticates when x-api-key is enabled
	tokenWithXAPIKey bool // the token wins only while x-api-key authentication is disabled
	oidc             bool // needs oidc-identities
	headers          map[string]string
	// identity is what the replaced token method returns, nil for API keys.
	identity map[string]any
	// username and group are the X-MaaS-Username and X-MaaS-Group values upstream. An
	// empty group means the header is not set.
	username, group string
}

func conditionsCredentials() []conditionsCredential {
	return []conditionsCredential{
		{
			name: "api key", authMethod: "api-keys", apiKey: true,
			headers:  map[string]string{"authorization": "Bearer " + conditionsFixtureAPIKey},
			username: "alice", group: `["team-a","team-b"]`,
		},
		{
			name: "api key in x-api-key", authMethod: "api-keys-x-api-key", apiKey: true, xAPIKey: true,
			headers:  map[string]string{"x-api-key": conditionsFixtureAPIKey},
			username: "alice", group: `["team-a","team-b"]`,
		},
		{
			// The Authorization key authenticates first, with or without x-api-key authentication.
			name: "api key next to another key in x-api-key", authMethod: "api-keys", apiKey: true,
			headers: map[string]string{
				"authorization": "Bearer " + conditionsFixtureAPIKey,
				"x-api-key":     "sk-oai-FAKEFAKEFAKEFAKEFAKEFAKE9876543210",
			},
			username: "alice", group: `["team-a","team-b"]`,
		},
		{
			// With x-api-key authentication on, the key authenticates before the token.
			name: "api key in x-api-key next to an openshift token", authMethod: "api-keys-x-api-key", apiKey: true, xAPIKey: true,
			headers:  map[string]string{"authorization": "Bearer sha256~FAKEFAKEFAKH", "x-api-key": conditionsFixtureAPIKey},
			username: "alice", group: `["team-a","team-b"]`,
		},
		{
			name: "openshift token", authMethod: "openshift-identities",
			headers: map[string]string{"authorization": "Bearer sha256~FAKEFAKEFAKE"},
			identity: map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": "system:serviceaccount:team-a:runner",
					"groups":   []any{"system:serviceaccounts", "team-a"},
				},
			},
			username: "system:serviceaccount:team-a:runner",
			group:    `["system:authenticated","system:serviceaccounts","team-a"]`,
		},
		{
			// "sk-oai-" inside the token, the account name and a non-key x-api-key must not
			// count as an API key: the guards anchor the prefix. Token alphabets (base64url)
			// include "-", so a token can contain it.
			name: "openshift token with sk-oai- in the token, account and x-api-key", authMethod: "openshift-identities",
			headers: map[string]string{"authorization": "Bearer sha256~sk-oai-FAKEFAKEF", "x-api-key": "xsk-oai-1"},
			identity: map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": "system:serviceaccount:team-a:sk-oai-runner",
					"groups":   []any{"system:serviceaccounts", "team-a"},
				},
			},
			username: "system:serviceaccount:team-a:sk-oai-runner",
			group:    `["system:authenticated","system:serviceaccounts","team-a"]`,
		},
		{
			// With x-api-key authentication disabled the token is the credential, so the
			// API-key headers must ignore the x-api-key header.
			name: "openshift token with an API key in x-api-key", authMethod: "openshift-identities", tokenWithXAPIKey: true,
			headers: map[string]string{"authorization": "Bearer sha256~FAKEFAKEFAKG", "x-api-key": conditionsFixtureAPIKey},
			identity: map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": "system:serviceaccount:team-a:runner",
					"groups":   []any{"system:serviceaccounts", "team-a"},
				},
			},
			username: "system:serviceaccount:team-a:runner",
			group:    `["system:authenticated","system:serviceaccounts","team-a"]`,
		},
		{
			name: "oidc token", authMethod: "oidc-identities", oidc: true,
			headers: map[string]string{"authorization": "Bearer aaaa.bbbb.cccc"},
			identity: map[string]any{
				"iss": conditionsFixtureIssuer, "sub": "5f1c", "preferred_username": "bob",
				"azp": "maas-client", "aud": "maas-api", "token_use": "access",
				"groups": []any{"team-a"},
			},
			username: "bob", group: `["system:authenticated","team-a"]`,
		},
		{
			// Organization-scoped tokens carry no group header.
			name: "oidc token with org_id", authMethod: "oidc-identities", oidc: true,
			headers: map[string]string{"authorization": "Bearer aaaa.bbbb.dddd"},
			identity: map[string]any{
				"iss": conditionsFixtureIssuer, "sub": "5f1d", "preferred_username": "carol",
				"azp": "maas-client", "aud": "maas-api", "token_use": "access",
				"groups": []any{"team-a"}, "org_id": conditionsFixtureOrgID,
			},
			username: "carol",
		},
	}
}

// conditionsCredentialsFor are the credentials a gateway with these methods authenticates.
func conditionsCredentialsFor(oidcOn, xAPIKeyOn bool) []conditionsCredential {
	return slices.DeleteFunc(conditionsCredentials(), func(credential conditionsCredential) bool {
		return (credential.xAPIKey && !xAPIKeyOn) || (credential.oidc && !oidcOn) || (credential.tokenWithXAPIKey && xAPIKeyOn)
	})
}

type conditionsRoute struct {
	name    string
	path    string
	headers map[string]string
	model   bool // subscription-info runs, so it is present on every allowed request
}

func conditionsRoutes() []conditionsRoute {
	return []conditionsRoute{
		{name: "path-routed model", path: conditionsFixtureModelPath, model: true},
		{
			name: "body-routed model", path: "/v1/chat/completions", model: true,
			headers: map[string]string{"x-gateway-model-name": "publishers/llm/models/qwen3"},
		},
		{name: "model listing", path: "/v1/models"},
		{name: "maas-api", path: "/maas-api/v1/models"},
	}
}

type conditionsSelection struct {
	name   string
	fields map[string]any
}

func conditionsSelections() []conditionsSelection {
	full := map[string]any{
		"name": "gold", "namespace": "models-as-a-service", "phase": "Active",
		"accessAllowed": true, "resolvedModel": "llm/qwen3",
		"costCenter": "cc-1234", "rateLimitId": "0123456789abcdef",
		"organizationId": conditionsFixtureOrgID, "objective": "gold-qwen3",
		"fairnessId":              "models-as-a-service",
		"selectedGuardrailChecks": map[string]any{"pii": true},
		"headers":                 map[string]any{conditionsFixtureSubHeader: []any{"team-a"}},
	}
	// maas-api omits empty optional fields (omitempty). selectedGuardrailChecks stays: with
	// a guardrail resolver maas-api sends a map even when no check applies, and
	// guardrail-checks-known denies a selection without one.
	minimal := map[string]any{
		"name": "bronze", "namespace": "models-as-a-service", "phase": "Active",
		"accessAllowed": true, "selectedGuardrailChecks": map[string]any{},
	}
	// maas-api omits rateLimitId and costCenter independently of each other: rateLimitId for a
	// subscription on the legacy rate-limit identity, costCenter when none is set.
	withoutRateLimitID := maps.Clone(full)
	delete(withoutRateLimitID, "rateLimitId")
	withoutCostCenter := maps.Clone(full)
	delete(withoutCostCenter, "costCenter")
	return []conditionsSelection{
		{name: "full selection", fields: full},
		{name: "minimal selection", fields: minimal},
		{name: "selection without rateLimitId", fields: withoutRateLimitID},
		{name: "selection without costCenter", fields: withoutCostCenter},
	}
}

// conditionsAPIKeyValidation is maas-api's validate response for the fixture key.
func conditionsAPIKeyValidation() map[string]any {
	return map[string]any{
		"valid": true, "userId": "c0ffee00-1111-2222-3333-444455556666",
		"username": "alice", "groups": []any{"team-a", "team-b"},
		"groupsJSON": `["team-a","team-b"]`,
		"keyId":      "c0ffee00-aaaa-bbbb-cccc-ddddeeeeffff", "keyName": "laptop",
		"subscription": "gold", "tenant": "",
	}
}

type conditionsRequest struct {
	name       string
	credential conditionsCredential
	route      conditionsRoute
	selection  *conditionsSelection
	request    gatewayRequest
	replies    maasAPIReplies
}

func newConditionsRequest(credential conditionsCredential, route conditionsRoute, selection *conditionsSelection) conditionsRequest {
	headers := map[string]string{"content-type": "application/json", "x-maas-subscription": "gold"}
	maps.Copy(headers, credential.headers)
	maps.Copy(headers, route.headers)
	replies := maasAPIReplies{}
	if credential.apiKey {
		replies[validateEndpoint] = conditionsAPIKeyValidation()
	}
	name := credential.name + " on " + route.name
	if selection != nil {
		replies[selectEndpoint] = selection.fields
		name += " with " + selection.name
	}
	return conditionsRequest{
		name:       name,
		credential: credential,
		route:      route,
		selection:  selection,
		request:    gatewayRequest{path: route.path, headers: headers, identity: credential.identity, authMethod: credential.authMethod},
		replies:    replies,
	}
}

// conditionsRequests is every allowed request shape the gateway serves for a spec, with
// maas-api's replies: apiKeyValidation for API keys, and subscription-info on model routes.
func conditionsRequests(oidcOn, xAPIKeyOn bool) []conditionsRequest {
	var requests []conditionsRequest
	for _, credential := range conditionsCredentialsFor(oidcOn, xAPIKeyOn) {
		for _, route := range conditionsRoutes() {
			if !route.model {
				requests = append(requests, newConditionsRequest(credential, route, nil))
				continue
			}
			for _, selection := range conditionsSelections() {
				requests = append(requests, newConditionsRequest(credential, route, &selection))
			}
		}
	}
	return requests
}

// conditionsEvaluate sends request, plus extraHeaders, through a gateway of its own and
// checks which method authenticated it.
func conditionsEvaluate(t *testing.T, spec map[string]any, request conditionsRequest, extraHeaders map[string]string) gatewayOutcome {
	t.Helper()
	sent := request.request
	sent.headers = maps.Clone(sent.headers)
	maps.Copy(sent.headers, extraHeaders)
	outcome := newAuthorinoGateway(t, spec, request.replies).evaluate(t, sent)
	if outcome.authenticatedBy != request.credential.authMethod {
		t.Errorf("authenticated by %q, want %q", outcome.authenticatedBy, request.credential.authMethod)
	}
	return outcome
}

type conditionsSpecVariant struct {
	name      string
	oidc      *oidcConfig
	xAPIKeyOn bool
}

func conditionsSpecVariants() []conditionsSpecVariant {
	oidc := &oidcConfig{
		IssuerURL: conditionsFixtureIssuer, ClientID: "maas-client", Audience: "maas-api",
		AccessTokenPurposeClaim: "token_use", AccessTokenPurpose: "access", TTL: 300,
	}
	return []conditionsSpecVariant{
		{name: "api keys only"},
		{name: "oidc", oidc: oidc},
		{name: "oidc and x-api-key", oidc: oidc, xAPIKeyOn: true},
	}
}

// conditionsSpec is the gateway AuthPolicy spec as the reconciler writes it.
func conditionsSpec(t *testing.T, variant conditionsSpecVariant, subscriptionHeaders []string) map[string]any {
	t.Helper()
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   celTestInfraNamespace,
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "gateway-ns",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
	}
	guardrails := []guardrailHeader{
		{ID: "pii", Name: conditionsFixtureGuardrailA},
		{ID: "toxicity", Name: conditionsFixtureGuardrailB},
	}
	spec := r.buildGatewayAuthPolicySpec(
		variant.oidc, variant.xAPIKeyOn, "", "models-as-a-service", "gateway-ns", "maas-default-gateway", guardrails,
	)
	if err := applySubscriptionHeaders(spec, subscriptionHeaders); err != nil {
		t.Fatalf("applySubscriptionHeaders: %v", err)
	}
	return spec
}

// TestGatewayAuthPolicyDeniesClientIdentityHeaders sends each gateway-owned header on its
// own. The deny rule has no `when` and reads only request headers, so each header goes with
// every credential the variant accepts on a model route, where every authorization rule runs.
// TestGatewayAuthPolicyAllowedRequests covers the other request shapes.
func TestGatewayAuthPolicyDeniesClientIdentityHeaders(t *testing.T) {
	t.Parallel()
	denied := []string{
		"x-maas-username",
		"x-maas-group",
		"x-maas-exact-groups",
		"x-maas-keyname",
		"x-maas-subscription-rate-limit-id",
		"x-maas-organization-id",
		"x-maas-cost-center",
		"x-maas-identity",
		"x-maas-admin-scope",
		"x-aigateway-guardrail-pii",
		"x-aigateway-guardrail-anything",
		"x-grid-unconfigured",
	}
	allowed := []string{
		conditionsFixtureSubHeader,
		"x-maas-subscription",
		"x-maas-subscription-namespace",
		"x-maas-key-id",
		"x-gateway-model-name",
		"x-grid",
		"x-aigateway-guardrail",
		"x-request-id",
		// Names are matched exactly.
		"x-maas-username-extra",
		"x-maas-keyname2",
	}
	type headerCase struct {
		header, value string
		wantAllowed   bool
	}
	cases := []headerCase{{wantAllowed: true}}
	for _, header := range denied {
		cases = append(cases, headerCase{header: header, value: "forged"}, headerCase{header: header, value: ""})
	}
	for _, header := range allowed {
		cases = append(cases, headerCase{header: header, value: "value", wantAllowed: true})
	}
	route := conditionsRoutes()[0]
	selection := conditionsSelections()[0]

	for _, variant := range conditionsSpecVariants() {
		spec := conditionsSpec(t, variant, []string{conditionsFixtureSubHeader})
		for _, credential := range conditionsCredentialsFor(variant.oidc != nil, variant.xAPIKeyOn) {
			request := newConditionsRequest(credential, route, &selection)
			for _, tc := range cases {
				name := "no extra header"
				extraHeaders := map[string]string{}
				if tc.header != "" {
					name = tc.header + "=" + tc.value
					extraHeaders[tc.header] = tc.value
				}
				t.Run(variant.name+"/"+credential.name+"/"+name, func(t *testing.T) {
					t.Parallel()

					// when
					outcome := conditionsEvaluate(t, spec, request, extraHeaders)

					// then
					if outcome.allowed != tc.wantAllowed {
						t.Fatalf("allowed = %v (status %v, body %q), want %v", outcome.allowed, outcome.status, outcome.body, tc.wantAllowed)
					}
					if !tc.wantAllowed {
						assertDenyResponse(t, outcome, conditionsAccessDenied, "unauthorized")
						if outcome.deniedBy != "deny-client-identity-headers" {
							t.Errorf("denied by %q, want deny-client-identity-headers", outcome.deniedBy)
						}
					}
				})
			}
		}
	}
}

func TestGatewayAuthPolicyDeniesSubscriptionHeadersWhenNoneConfigured(t *testing.T) {
	spec := conditionsSpec(t, conditionsSpecVariants()[0], nil)
	selection := conditionsSelections()[0]
	request := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[0], &selection)

	if outcome := conditionsEvaluate(t, spec, request, nil); !outcome.allowed {
		t.Fatalf("request without %s denied (%v, %q), want allowed", conditionsFixtureSubHeader, outcome.status, outcome.body)
	}
	outcome := conditionsEvaluate(t, spec, request, map[string]string{conditionsFixtureSubHeader: "value"})
	if outcome.allowed {
		t.Fatalf("%s allowed with no subscription headers configured, want denied", conditionsFixtureSubHeader)
	}
	assertDenyResponse(t, outcome, conditionsAccessDenied, "unauthorized")
	if outcome.deniedBy != "deny-client-identity-headers" {
		t.Errorf("denied by %q, want deny-client-identity-headers", outcome.deniedBy)
	}
}

// Every configured x-grid-* name is allowed on its own, not only the first, and an
// unconfigured one is denied next to an allowed one.
func TestGatewayAuthPolicyAllowsEveryConfiguredSubscriptionHeader(t *testing.T) {
	t.Parallel()
	configured := []string{conditionsFixtureSubHeader, "x-grid-region"}
	spec := conditionsSpec(t, conditionsSpecVariants()[0], configured)
	selection := conditionsSelections()[0]
	request := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[0], &selection)
	cases := []struct {
		name        string
		headers     map[string]string
		wantAllowed bool
	}{
		{name: "first configured name", headers: map[string]string{configured[0]: "a"}, wantAllowed: true},
		{name: "second configured name", headers: map[string]string{configured[1]: "a"}, wantAllowed: true},
		{name: "both configured names", headers: map[string]string{configured[0]: "a", configured[1]: "b"}, wantAllowed: true},
		{name: "configured and unconfigured names", headers: map[string]string{configured[1]: "a", "x-grid-unconfigured": "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// when
			outcome := conditionsEvaluate(t, spec, request, tc.headers)

			// then
			if outcome.allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v (status %v, body %q), want %v", outcome.allowed, outcome.status, outcome.body, tc.wantAllowed)
			}
			if !tc.wantAllowed {
				assertDenyResponse(t, outcome, conditionsAccessDenied, "unauthorized")
				if outcome.deniedBy != "deny-client-identity-headers" {
					t.Errorf("denied by %q, want deny-client-identity-headers", outcome.deniedBy)
				}
			}
		})
	}
}

// conditionsWantHeaders are the guarded success headers request gets upstream, by lower-case
// name. A header that maps to "" must not be set.
func conditionsWantHeaders(request conditionsRequest) map[string]string {
	want := map[string]string{
		"x-maas-username":                   request.credential.username,
		"x-maas-group":                      request.credential.group,
		"x-maas-keyname":                    "",
		"x-maas-subscription":               "",
		"x-maas-subscription-rate-limit-id": "",
		"x-maas-cost-center":                "",
		conditionsFixtureGuardrailA:         "",
		conditionsFixtureGuardrailB:         "",
	}
	if request.credential.apiKey {
		want["x-maas-keyname"] = "laptop"
		want["x-maas-subscription"] = "gold"
	}
	if request.selection != nil {
		fields := request.selection.fields
		want["x-maas-subscription-rate-limit-id"], _ = fields["rateLimitId"].(string)
		want["x-maas-cost-center"], _ = fields["costCenter"].(string)
		checks, _ := fields["selectedGuardrailChecks"].(map[string]any)
		for header, id := range map[string]string{conditionsFixtureGuardrailA: "pii", conditionsFixtureGuardrailB: "toxicity"} {
			want[header] = "false"
			if checks[id] == true {
				want[header] = "true"
			}
		}
	}
	return want
}

// The X-MaaS-Subscription-Rate-Limit-Id and X-MaaS-Cost-Center guards test for a non-empty
// value. maas-api sends both as omitempty strings; an empty string leaves the header out, and
// a value of another JSON type sets it.
func TestGatewayAuthPolicyOptionalFieldValues(t *testing.T) {
	t.Parallel()
	spec := conditionsSpec(t, conditionsSpecVariants()[0], []string{conditionsFixtureSubHeader})
	cases := []struct {
		name  string
		value any
		want  string // "" means the header is not set
	}{
		{name: "empty string", value: "", want: ""},
		{name: "number", value: 42, want: "42"},
		{name: "boolean", value: false, want: "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// given
			selection := conditionsSelections()[0]
			selection.fields = maps.Clone(selection.fields)
			selection.fields["rateLimitId"] = tc.value
			selection.fields["costCenter"] = tc.value
			request := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[0], &selection)

			// when
			outcome := conditionsEvaluate(t, spec, request, nil)

			// then
			if !outcome.allowed {
				t.Fatalf("denied (status %v, body %q), want allowed", outcome.status, outcome.body)
			}
			for _, name := range []string{"x-maas-subscription-rate-limit-id", "x-maas-cost-center"} {
				got, set := outcome.headers[name]
				switch {
				case tc.want == "" && set:
					t.Errorf("upstream header %s = %q, want it not set", name, got)
				case tc.want != "" && got != tc.want:
					t.Errorf("upstream header %s = %q (set: %v), want %q", name, got, set, tc.want)
				}
			}
		})
	}
}

// TestGatewayAuthPolicyAllowedRequests pins, for every allowed request shape, the maas-api
// calls and whether each guarded success header is set, with its value. evaluate also fails
// on a success header that runs and fails to resolve: Authorino drops it and cancels the
// response rules left in its priority, X-MaaS-* headers and the identity filter among them.
func TestGatewayAuthPolicyAllowedRequests(t *testing.T) {
	t.Parallel()
	for _, variant := range conditionsSpecVariants() {
		spec := conditionsSpec(t, variant, []string{conditionsFixtureSubHeader})
		for _, request := range conditionsRequests(variant.oidc != nil, variant.xAPIKeyOn) {
			t.Run(variant.name+"/"+request.name, func(t *testing.T) {
				t.Parallel()

				// when
				outcome := conditionsEvaluate(t, spec, request, nil)

				// then
				if !outcome.allowed {
					t.Fatalf("denied (status %v, body %q), want allowed", outcome.status, outcome.body)
				}
				// apiKeyValidation runs for API keys only, subscription-info on model routes only.
				var wantCalls []string
				if request.credential.apiKey {
					wantCalls = append(wantCalls, validateEndpoint)
				}
				if request.route.model {
					wantCalls = append(wantCalls, selectEndpoint)
				}
				if got := outcome.calledEndpoints(); !slices.Equal(got, wantCalls) {
					t.Errorf("maas-api calls = %v, want %v", got, wantCalls)
				}
				want := conditionsWantHeaders(request)
				for _, name := range slices.Sorted(maps.Keys(want)) {
					got, set := outcome.headers[name]
					switch {
					case want[name] == "" && set:
						t.Errorf("upstream header %s = %q, want it not set", name, got)
					case want[name] != "" && got != want[name]:
						t.Errorf("upstream header %s = %q (set: %v), want %q", name, got, set, want[name])
					}
				}
			})
		}
	}
}

// maas-api omits empty optional strings, so it never sends JSON null for them. If it did,
// the selector guards read null as "" and leave the header out, where the CEL guards they
// replace sent "<nil>". A null apiKeyValidation.subscription goes to the model listing:
// on a model route it fails the subscription-info cache key, on main too.
func TestGatewayAuthPolicyNullOptionalFieldsSetNoHeader(t *testing.T) {
	t.Parallel()
	spec := conditionsSpec(t, conditionsSpecVariants()[0], []string{conditionsFixtureSubHeader})
	selection := conditionsSelections()[0]
	selection.fields = maps.Clone(selection.fields)
	selection.fields["rateLimitId"] = nil
	selection.fields["costCenter"] = nil
	modelRequest := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[0], &selection)
	listingRequest := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[2], nil)
	validation := conditionsAPIKeyValidation()
	validation["subscription"] = nil
	listingRequest.replies[validateEndpoint] = validation
	cases := []struct {
		name    string
		request conditionsRequest
		headers []string
	}{
		{name: "subscription-info fields", request: modelRequest, headers: []string{"x-maas-subscription-rate-limit-id", "x-maas-cost-center"}},
		{name: "API-key subscription", request: listingRequest, headers: []string{"x-maas-subscription"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// when
			outcome := conditionsEvaluate(t, spec, tc.request, nil)

			// then
			if !outcome.allowed {
				t.Fatalf("denied (status %v, body %q), want allowed", outcome.status, outcome.body)
			}
			for _, name := range tc.headers {
				if value, set := outcome.headers[name]; set {
					t.Errorf("upstream header %s = %q, want it not set", name, value)
				}
			}
		})
	}
}

// A key in x-api-key authenticates nothing while x-api-key authentication is disabled, so
// maas-api never validates it.
func TestGatewayAuthPolicyIgnoresXAPIKeyWhenDisabled(t *testing.T) {
	for _, variant := range conditionsSpecVariants() {
		if variant.xAPIKeyOn {
			continue
		}
		t.Run(variant.name, func(t *testing.T) {
			// given
			gateway := newAuthorinoGateway(t, conditionsSpec(t, variant, []string{conditionsFixtureSubHeader}), nil)

			// when
			outcome := gateway.evaluate(t, gatewayRequest{
				path:    conditionsFixtureModelPath,
				headers: map[string]string{"x-api-key": conditionsFixtureAPIKey},
			})

			// then
			if outcome.allowed || outcome.status != envoytype.StatusCode_Unauthorized || outcome.authenticatedBy != "" {
				t.Errorf("allowed = %v, status = %v, authenticated by %q, want Unauthorized", outcome.allowed, outcome.status, outcome.authenticatedBy)
			}
			if calls := outcome.calledEndpoints(); len(calls) != 0 {
				t.Errorf("maas-api calls = %v, want none", calls)
			}
		})
	}
}

// selectedGuardrailHeader tests for subscription-info instead of the model-route predicate.
// That is only equivalent while allowed requests carry subscription-info exactly when they
// target a model: subscription-info runs on model routes, and guardrail-checks-known, under
// the same guard and never cached, denies a model request without a selection.
func TestGatewayAuthPolicyGuardrailGuardInvariant(t *testing.T) {
	t.Parallel()
	spec := conditionsSpec(t, conditionsSpecVariants()[0], nil)
	checksKnown := requiredMap(spec, "defaults", "rules", "authorization", "guardrail-checks-known")
	if _, cached := checksKnown["cache"]; cached {
		t.Error("guardrail-checks-known must not be cached: a cached allow could outlive a failed selection")
	}
	opa, _ := checksKnown["opa"].(map[string]any)
	rego, _ := opa["rego"].(string)
	if !strings.Contains(rego, `"selectedGuardrailChecks", null)`) || !strings.Contains(rego, "selected != null") {
		t.Errorf("guardrail-checks-known must deny a request without a selection, rego:\n%s", rego)
	}

	t.Run("model request without a guardrail selection is denied", func(t *testing.T) {
		// given
		selection := conditionsSelections()[0]
		selection.fields = maps.Clone(selection.fields)
		delete(selection.fields, "selectedGuardrailChecks")
		request := newConditionsRequest(conditionsCredentials()[0], conditionsRoutes()[0], &selection)

		// when
		outcome := conditionsEvaluate(t, spec, request, nil)

		// then
		if outcome.allowed {
			t.Fatal("allowed, want guardrail-checks-known to deny it")
		}
		assertDenyResponse(t, outcome, conditionsAccessDenied, "unauthorized")
		if outcome.deniedBy != "guardrail-checks-known" {
			t.Errorf("denied by %q, want guardrail-checks-known", outcome.deniedBy)
		}
	})

	for _, request := range conditionsRequests(false, false) {
		t.Run(request.name, func(t *testing.T) {
			t.Parallel()

			// when
			outcome := conditionsEvaluate(t, spec, request, nil)

			// then
			if !outcome.allowed {
				t.Fatalf("denied (status %v, body %q), want allowed", outcome.status, outcome.body)
			}
			if got := slices.Contains(outcome.calledEndpoints(), selectEndpoint); got != request.route.model {
				t.Errorf("subscription-info called = %v, want %v", got, request.route.model)
			}
			for _, name := range []string{conditionsFixtureGuardrailA, conditionsFixtureGuardrailB} {
				if _, got := outcome.headers[name]; got != request.route.model {
					t.Errorf("upstream header %s set = %v, want %v", name, got, request.route.model)
				}
			}
		})
	}
}
