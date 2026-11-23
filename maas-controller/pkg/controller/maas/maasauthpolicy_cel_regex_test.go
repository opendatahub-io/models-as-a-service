package maas

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	authorinocel "github.com/kuadrant/authorino/pkg/expressions/cel"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/groupname"
)

// The corpora below run one expression at a time through resolveForGateway
// rather than through the Authorino gateway pipeline, which costs too much per
// input for some 10,000 inputs.

// resolveForGateway compiles expression as compileForGateway does and returns a
// function that resolves it against an authorization JSON document the way
// Authorino's Expression.ResolveFor does. An evaluation error is returned as an
// error, never as false: gateway guards are negated and composed, and in CEL
// !error is still an error while !false is true. A document that can't be
// encoded fails the test, so it can't pass as an evaluation error.
func resolveForGateway(t *testing.T, expression string) func(document map[string]any) (any, error) {
	t.Helper()
	if err := compileForGateway(expression, nil); err != nil {
		t.Fatalf("compile %q: %v", expression, err)
	}
	program, err := authorinocel.Compile(expression, nil, gatewayCELLibraries...)
	if err != nil {
		t.Fatalf("compile %q: %v", expression, err)
	}
	return func(document map[string]any) (any, error) {
		t.Helper()
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode %#v: %v", document, err)
		}
		input, err := authorinocel.AuthJsonToCel(string(encoded))
		if err != nil {
			t.Fatalf("convert %s: %v", encoded, err)
		}
		value, _, err := program.Eval(input)
		if err != nil {
			return nil, err
		}
		resolved, err := authorinocel.ValueToJSON(value)
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal([]byte(resolved), &decoded); err != nil {
			t.Fatalf("decode %s: %v", resolved, err)
		}
		return decoded, nil
	}
}

// authorinoInput holds the request and auth roots of one evaluation. A nil
// root is left out of the document, so Authorino binds it to nil.
type authorinoInput struct {
	request map[string]any
	auth    map[string]any
}

func (in authorinoInput) document() map[string]any {
	document := map[string]any{}
	if in.request != nil {
		document["request"] = in.request
	}
	if in.auth != nil {
		document["auth"] = in.auth
	}
	return document
}

// authorinoOutcome is what resolveForGateway returns, with errors kept apart
// from false.
type authorinoOutcome struct {
	value any
	err   bool
}

func resolveOutcome(resolve func(map[string]any) (any, error), in authorinoInput) authorinoOutcome {
	value, err := resolve(in.document())
	if err != nil {
		return authorinoOutcome{err: true}
	}
	return authorinoOutcome{value: value}
}

func authorinoRequest(headerPairs ...string) map[string]any {
	headers := map[string]any{}
	for i := 0; i+1 < len(headerPairs); i += 2 {
		headers[headerPairs[i]] = headerPairs[i+1]
	}
	return map[string]any{"path": "/v1/models", "headers": headers}
}

func authWithIdentity(identity any) map[string]any {
	return map[string]any{"identity": identity}
}

// mismatchReporter reports the first few mismatches and then only the total,
// so a broken expression does not print one line per corpus entry.
type mismatchReporter struct {
	t     *testing.T
	count int
}

func (r *mismatchReporter) Errorf(format string, args ...any) {
	r.t.Helper()
	r.count++
	if r.count <= 5 {
		r.t.Errorf(format, args...)
	}
}

func (r *mismatchReporter) done() {
	r.t.Helper()
	if r.count > 5 {
		r.t.Errorf("%d mismatches in total", r.count)
	}
}

// regexFormCorpus returns deterministic, deduplicated inputs around every
// boundary of the regex forms, plus seeded random strings.
func regexFormCorpus() []string {
	org := identityTestOrganizationID
	var inputs []string

	// API key prefixes: every truncation, so a typo in a prefix literal shows.
	for _, prefix := range []string{"Bearer sk-oai-", "sk-oai-"} {
		for i := range len(prefix) + 1 {
			inputs = append(inputs, prefix[:i], prefix[:i]+"x", prefix[:i]+"x.y.z")
		}
	}
	inputs = append(inputs, "bearer sk-oai-x", "SK-OAI-x", " sk-oai-x", "Bearer  sk-oai-x",
		"Bearer sk-oai-\n", "x Bearer sk-oai-x", "Basic sk-oai-x")

	// JWT shape: empty parts at each position, part count, and the scheme at a
	// non-zero offset. The long headers are within Envoy's header size limit.
	inputs = append(inputs, "bearer a.b.c", "Bearer .", "Bearer ..", "Bearer ...", "Bearer a.b",
		"Bearer a.b.c", "Bearer  a.b.c", "Bearer a.b.c.", "Bearer a..c", "Bearer .b.c", "Bearer a.b.",
		"Bearer a.b.c.d", "Bearer a\n.b\n.c\n", "Basic Bearer a.b.c", " Bearer a.b.c",
		"Bearer "+strings.Repeat(".", 1000), "Bearer "+strings.Repeat("a.", 500),
		"Bearer "+strings.Repeat("a", 500)+"."+strings.Repeat("b", 500)+"."+strings.Repeat("c", 500))

	// Organization-qualified groups: a '/' at code point 36 with and without a
	// UUID before it, and code point 36 apart from byte 36.
	inputs = append(inputs, org, org+"/", org+"//", org+"/team", strings.ToUpper(org)+"/team",
		org[:35]+"/team", org+"x/team", "x"+org+"/team", "team/"+org, " "+org+"/team",
		strings.Repeat("a", 36)+"/team", strings.Repeat("é", 36)+"/team", strings.Repeat("é", 35)+"/team",
		"é"+org+"/team", "42c3e46-f2477-44d7-a85b-d3b43f6b31fa/team")
	for i := range len(org) {
		for _, c := range []string{"0", "g", "A", "-", "/", "é"} {
			inputs = append(inputs, org[:i]+c+org[i+1:]+"/team")
		}
	}

	// Group-name allowlist.
	inputs = append(inputs, "system:authenticated", "system:serviceaccounts:maas", "IAM#AccessGroupId-1",
		"a b", "a\tb", "a\nb", "a\x00b", "é", strings.Repeat("g", 256))

	// Every rune below U+0300, alone and after each prefix the forms test.
	for r := range rune(0x300) {
		c := string(r)
		inputs = append(inputs, c, "a"+c, "Bearer "+c, "Bearer sk-oai-"+c, "sk-oai-"+c,
			"Bearer "+c+".b.c", org+c+"team", org+"/"+c)
	}

	// Hex digits and '-' build UUID-like strings, '.' builds JWT-like ones, and
	// the rest sit on both sides of the allowlist, including 2- to 4-byte UTF-8.
	alphabet := []rune(":#._/ -\n\t\x00é€𝔸ZB0123456789abcdef")
	prefixes := []string{"", "Bearer ", "Bearer sk-oai-", "sk-oai-", org, org + "/"}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // seeded for a reproducible corpus
	for range 5000 {
		var b strings.Builder
		b.WriteString(prefixes[rng.IntN(len(prefixes))])
		for range rng.IntN(12) {
			b.WriteRune(alphabet[rng.IntN(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}

	slices.Sort(inputs)
	return slices.Compact(inputs)
}

// regexOrganizationQualifiedGroup is the celOrganizationQualifiedGroup pattern
// the guard was written for. It is spelled out so that a change to the
// production pattern shows here instead of moving the reference with it.
const regexOrganizationQualifiedGroup = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/`

// TestCELStringChecksMatchRegexForm checks each string-function guard against
// the equivalent matches() form: for every input both return the same value
// or both fail to evaluate.
func TestCELStringChecksMatchRegexForm(t *testing.T) {
	t.Parallel()
	const (
		regexBearerAPIKey = `request.headers.authorization.matches("^Bearer sk-oai-.*")`
		regexAuthzAPIKey  = `("authorization" in request.headers && request.headers["authorization"].matches("^Bearer sk-oai-.*"))`
		regexXAPIKey      = `("x-api-key" in request.headers && request.headers["x-api-key"].matches("^sk-oai-.*"))` //nolint:gosec // sk-oai- is the API key prefix, not a credential
		regexJWTShape     = `request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`
	)
	isAPIKey, isNotAPIKey, _ := apiKeyCELPredicates(false)
	xIsAPIKey, xIsNotAPIKey, xExtractKey := apiKeyCELPredicates(true)
	regexXIsAPIKey := regexAuthzAPIKey + ` || ` + regexXAPIKey

	authorizationOnly := func(s string) []authorinoInput {
		return []authorinoInput{{request: authorinoRequest("authorization", s)}, {request: authorinoRequest()}}
	}
	eitherHeader := func(s string) []authorinoInput {
		return []authorinoInput{
			{request: authorinoRequest("authorization", s)},
			{request: authorinoRequest("x-api-key", s)},
			{request: authorinoRequest("authorization", s, "x-api-key", s)},
			// Both headers carry an API key when s does, with different keys.
			{request: authorinoRequest("authorization", "Bearer "+s, "x-api-key", "sk-oai-"+s)},
			{request: authorinoRequest()},
		}
	}

	tests := []struct {
		name          string
		regex, actual string
		inputs        func(s string) []authorinoInput
	}{
		{
			name:   "API key in Authorization",
			regex:  regexBearerAPIKey,
			actual: isAPIKey,
			inputs: authorizationOnly,
		},
		{
			name:   "not an API key in Authorization",
			regex:  `!` + regexBearerAPIKey,
			actual: isNotAPIKey,
			inputs: authorizationOnly,
		},
		{
			name:   "API key in Authorization or x-api-key",
			regex:  regexXIsAPIKey,
			actual: xIsAPIKey,
			inputs: eitherHeader,
		},
		{
			name:   "not an API key, x-api-key enabled",
			regex:  `!(` + regexXIsAPIKey + `)`,
			actual: xIsNotAPIKey,
			inputs: eitherHeader,
		},
		{
			name: "raw API key, x-api-key enabled",
			regex: `(` + regexAuthzAPIKey + ` ? request.headers["authorization"].replace("Bearer ", "") : ` +
				regexXAPIKey + ` ? request.headers["x-api-key"] : "")`,
			actual: xExtractKey,
			inputs: eitherHeader,
		},
		{
			name:   "JWT shape",
			regex:  regexJWTShape,
			actual: authpolicy.ExternalJWTShape,
			inputs: func(s string) []authorinoInput {
				return append(authorizationOnly(s), authorinoInput{request: authorinoRequest("authorization", "Bearer "+s)})
			},
		},
		{
			// The guard only holds for string groups: every caller checks
			// type(g) == string before it, in the same all() or a preceding one.
			// TestTokenGroupsFollowGroupNameRule covers non-string groups
			// through the full group expressions.
			name:   "organization-qualified group",
			regex:  `auth.identity.groups.all(g, !g.matches('` + regexOrganizationQualifiedGroup + `'))`,
			actual: `auth.identity.groups.all(g, !` + celIsOrganizationQualifiedGroup + `)`,
			inputs: func(s string) []authorinoInput {
				return []authorinoInput{{auth: authWithIdentity(map[string]any{"groups": []any{s}})}}
			},
		},
	}

	corpus := regexFormCorpus()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			regex := resolveForGateway(t, test.regex)
			actual := resolveForGateway(t, test.actual)
			mismatches := &mismatchReporter{t: t}
			for _, s := range corpus {
				for _, in := range test.inputs(s) {
					want := resolveOutcome(regex, in)
					got := resolveOutcome(actual, in)
					if !reflect.DeepEqual(got, want) {
						mismatches.Errorf("request=%#v auth=%#v: got %+v, want %+v (matches form)", in.request, in.auth, got, want)
					}
				}
			}
			mismatches.done()
		})
	}
}

// TestTokenGroupsFollowGroupNameRule checks celGroups for each token identity
// against the rule it implements: a group is kept when groupname.Valid accepts
// it and it is not organization-qualified, and one bad group drops the list.
func TestTokenGroupsFollowGroupNameRule(t *testing.T) {
	t.Parallel()
	org := identityTestOrganizationID
	organizationQualified := regexp.MustCompile(regexOrganizationQualifiedGroup)
	identities := []struct {
		name     string
		identity func(groups []any) map[string]any
		qualify  func(g string) any
	}{
		{
			name:     "TokenReview",
			identity: func(groups []any) map[string]any { return map[string]any{"user": map[string]any{"groups": groups}} },
			qualify:  func(g string) any { return g },
		},
		{
			name: "OIDC",
			identity: func(groups []any) map[string]any {
				return map[string]any{"iss": "https://issuer.example.com", "groups": groups}
			},
			qualify: func(g string) any { return g },
		},
		{
			name: "OIDC with org_id",
			identity: func(groups []any) map[string]any {
				return map[string]any{"iss": "https://issuer.example.com", "org_id": org, "groups": groups}
			},
			qualify: func(g string) any { return org + "/" + g },
		},
	}

	nonStrings := []any{float64(42), true, nil, []any{}, []any{"a"}, map[string]any{}, map[string]any{"a": "b"}}
	corpus := regexFormCorpus()
	for _, id := range identities {
		t.Run(id.name, func(t *testing.T) {
			t.Parallel()
			resolveGroups := resolveForGateway(t, celGroups)
			mismatches := &mismatchReporter{t: t}
			check := func(groups []any, want []any) {
				t.Helper()
				in := authorinoInput{request: authorinoRequest(), auth: authWithIdentity(id.identity(groups))}
				got := resolveOutcome(resolveGroups, in)
				if !reflect.DeepEqual(got, authorinoOutcome{value: want}) {
					mismatches.Errorf("groups %#v: got %+v, want %#v", groups, got, want)
				}
			}
			for _, s := range corpus {
				want := []any{}
				if groupname.Valid(s) && !organizationQualified.MatchString(s) {
					want = []any{id.qualify(s)}
				}
				check([]any{s}, want)
			}
			for _, g := range nonStrings {
				check([]any{g}, []any{})
				check([]any{"users", g}, []any{})
			}
			check([]any{"users", org + "/team"}, []any{})
			check([]any{"users", org + "/"}, []any{})
			mismatches.done()
		})
	}
}

// TestGatewayAuthPolicyTellsAPIKeysFromJWTs runs a policy with OIDC and x-api-key
// both enabled, where the string checks decide whether a request carries an API
// key and so whether its JWT is authenticated. Every request carries claims the
// oidc-identities method would accept, so a case shows which method wins.
func TestGatewayAuthPolicyTellsAPIKeysFromJWTs(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "maas-system",
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "gateway-ns",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
		AuthzCacheTTL:    60,
	}
	oidc := &oidcConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-client",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
		TTL:                     300,
	}
	spec := r.buildGatewayAuthPolicySpec(oidc, true, "", "models-as-a-service", "gateway-ns", "maas-default-gateway", []guardrailHeader{})

	const jwt = "Bearer aaa.bbb.ccc"
	request := func(path string, headers map[string]string) gatewayRequest {
		if path == "" {
			path = celTestModelPath
		}
		// authMethod only gives the OIDC claims to oidc-identities; wantMethod is
		// the method expected to authenticate.
		return gatewayRequest{path: path, headers: headers, identity: celTestOIDCClaims(), authMethod: "oidc-identities"}
	}
	validKey := maasAPIReplies{
		validateEndpoint: celTestAPIKeyValidation("system:authenticated"),
		selectEndpoint:   celTestSubscriptionInfo(),
	}
	apiKeyHeaders := map[string]string{"x-maas-username": "alice", "x-maas-group": `["system:authenticated"]`}
	oidcHeaders := map[string]string{"x-maas-username": "alice", "x-maas-organization-id": celTestOrganizationID}

	tests := []struct {
		name          string
		path          string // a model route when empty
		headers       map[string]string
		maasAPI       maasAPIReplies
		wantMethod    string
		wantAllowed   bool
		wantCalls     []string
		wantDeniedBy  string // set only when one rule alone denies
		wantHeaders   map[string]string
		wantNoHeaders []string
	}{
		{
			name:        "API key in Authorization",
			headers:     map[string]string{"authorization": "Bearer sk-oai-key"},
			maasAPI:     validKey,
			wantMethod:  "api-keys",
			wantAllowed: true,
			wantCalls:   []string{validateEndpoint, selectEndpoint},
			wantHeaders: apiKeyHeaders,
		},
		{
			// Without Authorization only the x-api-key method can authenticate, and
			// the X-MaaS-Identity and oidc-client-bound predicates are false because
			// they check for the header first.
			name:          "API key in x-api-key",
			headers:       map[string]string{"x-api-key": "sk-oai-key"},
			maasAPI:       validKey,
			wantMethod:    "api-keys-x-api-key",
			wantAllowed:   true,
			wantCalls:     []string{validateEndpoint, selectEndpoint},
			wantHeaders:   apiKeyHeaders,
			wantNoHeaders: []string{"x-maas-identity"},
		},
		{
			name:          "API key in x-api-key on a maas-api route",
			path:          "/maas-api/v1/models",
			headers:       map[string]string{"x-api-key": "sk-oai-key"},
			maasAPI:       maasAPIReplies{validateEndpoint: celTestAPIKeyValidation("system:authenticated")},
			wantMethod:    "api-keys-x-api-key",
			wantAllowed:   true,
			wantCalls:     []string{validateEndpoint},
			wantHeaders:   apiKeyHeaders,
			wantNoHeaders: []string{"x-maas-identity"},
		},
		{
			name:          "API key in x-api-key on /v1/me",
			path:          "/v1/me",
			headers:       map[string]string{"x-api-key": "sk-oai-key"},
			maasAPI:       maasAPIReplies{validateEndpoint: celTestAPIKeyValidation("system:authenticated")},
			wantMethod:    "api-keys-x-api-key",
			wantAllowed:   true,
			wantCalls:     []string{validateEndpoint},
			wantHeaders:   apiKeyHeaders,
			wantNoHeaders: []string{"x-maas-identity"},
		},
		{
			name:          "JWT in Authorization",
			headers:       map[string]string{"authorization": jwt},
			maasAPI:       maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantMethod:    "oidc-identities",
			wantAllowed:   true,
			wantCalls:     []string{selectEndpoint},
			wantHeaders:   oidcHeaders,
			wantNoHeaders: []string{"x-maas-group"},
		},
		{
			name:        "API key in x-api-key next to a JWT in Authorization",
			headers:     map[string]string{"authorization": jwt, "x-api-key": "sk-oai-key"},
			maasAPI:     validKey,
			wantMethod:  "api-keys-x-api-key",
			wantAllowed: true,
			wantCalls:   []string{validateEndpoint, selectEndpoint},
			wantHeaders: apiKeyHeaders,
		},
		{
			name:        "API key in Authorization next to a JWT-shaped x-api-key",
			headers:     map[string]string{"authorization": "Bearer sk-oai-key", "x-api-key": "aaa.bbb.ccc"},
			maasAPI:     validKey,
			wantMethod:  "api-keys",
			wantAllowed: true,
			wantCalls:   []string{validateEndpoint, selectEndpoint},
			wantHeaders: apiKeyHeaders,
		},
		{
			// api-keys runs first, so the Authorization key is the one validated.
			name:        "API keys in Authorization and x-api-key",
			headers:     map[string]string{"authorization": "Bearer sk-oai-key", "x-api-key": "sk-oai-other"},
			maasAPI:     validKey,
			wantMethod:  "api-keys",
			wantAllowed: true,
			wantCalls:   []string{validateEndpoint, selectEndpoint},
			wantHeaders: apiKeyHeaders,
		},
		{
			name:         "API keys in Authorization and x-api-key on the key-management API",
			path:         "/maas-api/v1/api-keys",
			headers:      map[string]string{"authorization": "Bearer sk-oai-key", "x-api-key": "sk-oai-other"},
			maasAPI:      maasAPIReplies{validateEndpoint: celTestAPIKeyValidation("system:authenticated")},
			wantMethod:   "api-keys",
			wantCalls:    []string{validateEndpoint},
			wantDeniedBy: "deny-api-key-management",
		},
		{
			// The JWT must not authenticate a request whose API key failed validation.
			name:       "invalid API key in x-api-key next to a JWT in Authorization",
			headers:    map[string]string{"authorization": jwt, "x-api-key": "sk-oai-key"},
			maasAPI:    maasAPIReplies{validateEndpoint: celTestInvalidAPIKey},
			wantMethod: "api-keys-x-api-key",
			wantCalls:  []string{validateEndpoint},
		},
		{
			name:          "JWT in Authorization next to an x-api-key that is not an API key",
			headers:       map[string]string{"authorization": jwt, "x-api-key": "not-a-key"},
			maasAPI:       maasAPIReplies{selectEndpoint: celTestSubscriptionInfo()},
			wantMethod:    "oidc-identities",
			wantAllowed:   true,
			wantCalls:     []string{selectEndpoint},
			wantHeaders:   oidcHeaders,
			wantNoHeaders: []string{"x-maas-group"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// given
			gateway := newAuthorinoGateway(t, spec, test.maasAPI)

			// when
			outcome := gateway.evaluate(t, request(test.path, test.headers))

			// then
			if outcome.authenticatedBy != test.wantMethod {
				t.Errorf("authenticated by %q, want %q", outcome.authenticatedBy, test.wantMethod)
			}
			if got := outcome.calledEndpoints(); !slices.Equal(got, test.wantCalls) {
				t.Errorf("maas-api calls = %v, want %v", got, test.wantCalls)
			}
			// Every case sends sk-oai-key in the header that should win.
			if slices.Contains(test.wantCalls, validateEndpoint) {
				if got := outcome.callBody(validateEndpoint); !reflect.DeepEqual(got, map[string]any{"key": "sk-oai-key"}) {
					t.Errorf("validate body = %v, want key sk-oai-key", got)
				}
			}
			if test.wantDeniedBy != "" && outcome.deniedBy != test.wantDeniedBy {
				t.Errorf("denied by %q, want %q", outcome.deniedBy, test.wantDeniedBy)
			}
			if outcome.allowed != test.wantAllowed {
				t.Fatalf("allowed = %v (status %v, body %q), want %v", outcome.allowed, outcome.status, outcome.body, test.wantAllowed)
			}
			if !test.wantAllowed {
				assertDenyResponse(t, outcome, `{"error":{"message":"Access denied","type":"authorization_error","code":403}}`, "unauthorized")
				return
			}
			for name, want := range test.wantHeaders {
				if got, ok := outcome.headers[name]; !ok || got != want {
					t.Errorf("upstream header %s = %q (sent: %v), want %q", name, got, ok, want)
				}
			}
			for _, name := range test.wantNoHeaders {
				if got, ok := outcome.headers[name]; ok {
					t.Errorf("upstream header %s = %q, want it skipped", name, got)
				}
			}
		})
	}
}

// allowedCELRegex lists the only matches() calls the gateway AuthPolicy CEL may
// make. Authorino compiles each pattern on every call, so a new one costs every
// request that reaches it. The guard is spelled out rather than taken from
// celIsOrganizationQualifiedGroup, so that removing it fails the check. Every
// entry must be used by some gateway AuthPolicy, so that an entry left behind
// cannot let its matches() call come back unnoticed.
//
// Only CEL is checked. Structured `operator: matches` conditions stay: Authorino
// reads them with gjson, while a CEL predicate would parse the whole
// authorization JSON first.
var allowedCELRegex = []string{
	`(size(g) > 36 && g.charAt(36) == "/" && g.matches('` + celOrganizationQualifiedGroup + `'))`,
	// One cheap compile per group. A regex-free allowlist costs one CEL step per
	// character and is slower for group names over about 16 characters.
	`g.matches('` + safeGroupNamePattern + `')`,
	// Runs once per evaluation, not per group, and a valid org_id has to pass
	// the full pattern anyway.
	`auth.identity["org_id"].matches('` + celCanonicalOrganizationIDPattern + `')`,
}

// assertOnlyAllowedRegexCEL fails for any matches() call outside allowedCELRegex.
func assertOnlyAllowedRegexCEL(t *testing.T, occurrences []authPolicyCELOccurrence) {
	t.Helper()
	for _, occ := range occurrences {
		expr := occ.Expr
		for _, allowed := range allowedCELRegex {
			expr = strings.ReplaceAll(expr, allowed, "")
		}
		if strings.Contains(expr, "matches(") {
			t.Errorf("CEL at %s calls matches() outside allowedCELRegex: %q", occ.Path, occ.Expr)
		}
	}
}
