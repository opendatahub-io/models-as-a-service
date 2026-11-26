/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	envoyauth "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	envoytype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/gogo/googleapis/google/rpc"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/parser"
	authorinoapi "github.com/kuadrant/authorino/api/v1beta3"
	authorinocontrollers "github.com/kuadrant/authorino/controllers"
	"github.com/kuadrant/authorino/pkg/auth"
	"github.com/kuadrant/authorino/pkg/evaluators"
	authorinocel "github.com/kuadrant/authorino/pkg/expressions/cel"
	"github.com/kuadrant/authorino/pkg/index"
	"github.com/kuadrant/authorino/pkg/jsonexp"
	authorinolog "github.com/kuadrant/authorino/pkg/log"
	authorinoservice "github.com/kuadrant/authorino/pkg/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newAuthorinoGateway runs a generated gateway AuthPolicy on Authorino itself:
// Authorino's reconciler translates the rules into an AuthConfig, and its auth pipeline
// evaluates each request against a fake maas-api. Evaluation order, `when` conditions,
// OPA rules, metadata calls and response rendering are Authorino's.
//
// Authorino doesn't fail a request when CEL errors: a deny body or header that errors
// is sent empty, a success header or filter that errors is dropped and cancels the rest
// of its priority group, and a `when` or cache key that errors skips its evaluator or
// the cache without a log line. The harness reports these, and the metadata and
// authorization failures Authorino logs, as gatewayOutcome.failures; evaluate fails the
// test on any of them.
//
// Not covered, so these need a test of their own:
//   - Authentication other than API keys. Each such method is replaced by a plain
//     identity read from its own header, testIdentityHeader + "-" + the method name, so
//     a request authenticates only through the method it names. The method's `when`,
//     priority, overrides and defaults stay.
//   - Kuadrant's translation of the AuthPolicy into an AuthConfig, apart from the
//     rename of the success filters to dynamicMetadata.
//   - The wasm-shim, including spec.defaults.when.
//   - Deny bodies and headers that error. Authorino sends them empty, so the cases
//     assert their values.
//   - A `when` of an evaluator Authorino cancels before evaluating it, once another
//     evaluator in its priority group has decided the phase.
//   - An authorization error in a rule whose sibling at the same priority denies first.
//     Authorino reports only the first failure and cancels the rest, so such an error
//     shows up by chance; give each erroring rule a case where it alone can fail.
//   - A cache that fails to store an entry, or a cache key that resolves to null.
//     Authorino evaluates without the cache and logs nothing.
//   - Authorino as RHCL builds it. The pipeline is Authorino v0.26.1, the RHCL 1.4 line,
//     built with this module's dependencies, including cel-go v0.29.2. Only the compile
//     check holds CEL to the gateway's libraries and syntax.

// gatewayCELLibraries are the CEL library versions Authorino gets from cel-go v0.21.0,
// which v0.24.x to v0.26.x build with (RHCL 1.3 and 1.4). v0.28.0 (RHCL 1.5) builds
// with cel-go v0.29.0 and gets newer libraries, so the pins are conservative there.
// This module builds with cel-go v0.29.2, so Authorino would pick up newer libraries
// with functions the gateway lacks. cel-go keeps the first registration of a library,
// so passing these to Compile ahead of Authorino's own pins them.
// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/expressions/cel/expressions.go#L90-L125
var gatewayCELLibraries = []cel.EnvOption{
	ext.Strings(ext.StringsVersion(3)),
	cel.OptionalTypes(cel.OptionalTypesVersion(1)),
}

// gatewayParser parses with the syntax of cel-go v0.21.0. cel-go v0.29.2 also accepts
// backtick-quoted identifiers by default, which the gateway rejects as a syntax error,
// and no environment option turns them off.
// https://github.com/google/cel-go/blob/v0.29.2/parser/parser.go#L45
var gatewayParser = func() *parser.Parser {
	p, err := parser.NewParser(parser.EnableOptionalSyntax(true), parser.EnableIdentEscapeSyntax(false))
	if err != nil {
		panic(err)
	}
	return p
}()

// compiledForGateway memoizes compileForGateway, which every gateway runs on every
// expression of its policy.
var compiledForGateway sync.Map

func compileForGateway(expression string, outputType *cel.Type) error {
	key := fmt.Sprintf("%v\x00%s", outputType, expression)
	if cached, ok := compiledForGateway.Load(key); ok {
		err, _ := cached.(error)
		return err
	}
	var err error
	if _, issues := gatewayParser.Parse(common.NewTextSource(expression)); issues != nil && len(issues.GetErrors()) > 0 {
		err = fmt.Errorf("syntax the gateway lacks: %s", issues.ToDisplayString())
	} else {
		_, err = authorinocel.Compile(expression, outputType, gatewayCELLibraries...)
	}
	compiledForGateway.Store(key, err)
	return err
}

const (
	gatewayTestHost = "gateway.test"
	// testIdentityHeader, suffixed with "-" and a method name, carries the identity for an
	// authentication method the tests replace with a plain identity.
	testIdentityHeader = "x-test-identity"

	validateEndpoint = "/internal/v1/api-keys/validate"
	selectEndpoint   = "/internal/v1/subscriptions/select"
)

// maasAPIReplies holds maas-api's reply for each endpoint the gateway may call. Calling
// an endpoint without a reply fails the test, and so does a reply for an endpoint the
// policy never calls.
type maasAPIReplies map[string]any

type unreachableReply struct{}

// maasAPIUnreachable as a reply closes the connection, as an unreachable maas-api would.
var maasAPIUnreachable = unreachableReply{}

type maasAPICall struct {
	// scheme and host are those of the URL in the policy, before the harness points it
	// at the fake.
	scheme   string
	host     string
	endpoint string
	body     any
}

// fakeMaaSAPI answers the gateway's maas-api calls with a case's replies and records
// them. newAuthorinoGateway prefixes each metadata path with the URL's scheme and host,
// so the first two path segments are what the policy called. Its failures go to the
// gateway's log, so they reach gatewayOutcome.failures like Authorino's.
type fakeMaaSAPI struct {
	log     *pipelineLog
	mu      sync.Mutex
	replies maasAPIReplies
	calls   []maasAPICall
}

func (f *fakeMaaSAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scheme, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	host, endpoint, _ := strings.Cut(rest, "/")
	endpoint = "/" + endpoint
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.log.failf("read maas-api request body: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		f.log.failf("maas-api %s got a body that is not JSON: %q", endpoint, body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, maasAPICall{scheme: scheme, host: host, endpoint: endpoint, body: decoded})
	reply, ok := f.replies[endpoint]
	f.mu.Unlock()
	if !ok {
		f.log.failf("maas-api %s called without a reply in the case", endpoint)
	}
	if _, unreachable := reply.(unreachableReply); !ok || unreachable {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		f.log.failf("encode maas-api reply: %v", err)
	}
}

func (f *fakeMaaSAPI) recordedCalls() []maasAPICall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// pipelineLog collects the evaluator failures Authorino doesn't fail the request on: a
// `when`, cache key, metadata call or response config that fails, or an authorization
// rule that fails for a reason other than a denial.
type pipelineLog struct {
	// failingMetadata names the metadata evaluators whose maas-api is unreachable on
	// purpose, and names holds every evaluator's name. Both are read-only once
	// newAuthorinoGateway returns.
	failingMetadata map[string]bool
	names           map[auth.AuthConfigEvaluator]string

	mu       sync.Mutex
	failures []string
	// decided holds the evaluators past every check whose error Authorino swallows: the
	// `when`, then the cache key if there is one. Evaluators Authorino skipped because
	// their priority group was already decided count too. A decided evaluator may still
	// be running.
	decided map[auth.AuthConfigEvaluator]bool
	// authenticatedBy names the authentication methods Authorino validated an identity
	// with, and deniedBy the authorization rules that denied the request.
	authenticatedBy []string
	deniedBy        []string
}

func (l *pipelineLog) authenticated(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.authenticatedBy = append(l.authenticatedBy, name)
}

func (l *pipelineLog) denied(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deniedBy = append(l.deniedBy, name)
}

func (l *pipelineLog) recordedDecisions() (authenticatedBy, deniedBy []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.authenticatedBy), slices.Clone(l.deniedBy)
}

func (l *pipelineLog) failf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = append(l.failures, fmt.Sprintf(format, args...))
}

func (l *pipelineLog) recordedFailures() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.failures)
}

func (l *pipelineLog) decide(evaluator auth.AuthConfigEvaluator) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.decided[evaluator] = true
}

// undecided names the evaluators still undecided in the priority groups Authorino
// started. A phase returns as soon as one evaluator decides it, leaving the rest of the
// group running, so a `when` can fail after Evaluate returns. A group with no decided
// evaluator never started.
// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/service/auth_pipeline.go#L222-L245
func (l *pipelineLog) undecided(groups [][]auth.AuthConfigEvaluator) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var pending []string
	for _, group := range groups {
		var groupPending []string
		for _, evaluator := range group {
			if !l.decided[evaluator] {
				groupPending = append(groupPending, l.names[evaluator])
			}
		}
		if len(groupPending) != len(group) {
			pending = append(pending, groupPending...)
		}
	}
	return pending
}

// recordingConditions reports a `when` that fails to evaluate. Authorino skips the
// evaluator as if the conditions didn't match, and logs nothing.
// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/service/auth_pipeline.go#L123-L128
type recordingConditions struct {
	conditions jsonexp.Expression
	evaluator  auth.AuthConfigEvaluator
	name       string
	log        *pipelineLog
	// cached evaluators are decided when Authorino resolves their cache key, which it
	// does right after a matching `when`, on the evaluator's own goroutine.
	// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/service/auth_pipeline.go#L123-L136
	cached bool
}

func (c recordingConditions) Matches(authJSON string) (bool, error) {
	matched, err := true, error(nil)
	if c.conditions != nil {
		if matched, err = c.conditions.Matches(authJSON); err != nil {
			c.log.failf("when %s: %v", c.name, err)
		}
	}
	if c.evaluator != nil && (!matched || err != nil || !c.cached) {
		c.log.decide(c.evaluator)
	}
	return matched, err
}

// recordingCache reports a cache key that fails to resolve. Authorino evaluates without
// the cache, and logs nothing.
// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/evaluators/metadata.go#L54-L58
type recordingCache struct {
	evaluators.EvaluatorCache
	evaluator auth.AuthConfigEvaluator
	name      string
	log       *pipelineLog
}

func (c recordingCache) ResolveKeyFor(authJSON string) (any, error) {
	defer c.log.decide(c.evaluator)
	key, err := c.EvaluatorCache.ResolveKeyFor(authJSON)
	if err != nil {
		c.log.failf("cache key %s: %v", c.name, err)
	}
	return key, err
}

// instrument wraps the `when` and cache of every evaluator, and returns the evaluators
// grouped by phase and priority, as Authorino runs them.
func (l *pipelineLog) instrument(t *testing.T, config *evaluators.AuthConfig) [][]auth.AuthConfigEvaluator {
	t.Helper()
	phases := []struct {
		name       string
		evaluators []auth.AuthConfigEvaluator
	}{
		{"authentication", config.IdentityConfigs},
		{"metadata", config.MetadataConfigs},
		{"authorization", config.AuthorizationConfigs},
		{"response", config.ResponseConfigs},
	}
	config.Conditions = recordingConditions{conditions: config.Conditions, name: "auth config", log: l}
	var groups [][]auth.AuthConfigEvaluator
	for _, phase := range phases {
		byPriority := map[int][]auth.AuthConfigEvaluator{}
		for _, evaluator := range phase.evaluators {
			var name string
			var priority int
			var conditions *jsonexp.Expression
			var cache *evaluators.EvaluatorCache
			switch e := evaluator.(type) {
			case *evaluators.IdentityConfig:
				name, priority, conditions, cache = e.Name, e.Priority, &e.Conditions, &e.Cache
			case *evaluators.MetadataConfig:
				name, priority, conditions, cache = e.Name, e.Priority, &e.Conditions, &e.Cache
			case *evaluators.AuthorizationConfig:
				name, priority, conditions, cache = e.Name, e.Priority, &e.Conditions, &e.Cache
			case *evaluators.ResponseConfig:
				name, priority, conditions, cache = e.Name, e.Priority, &e.Conditions, &e.Cache
			default:
				t.Fatalf("%s evaluator %T is unknown, so its `when` can't be checked", phase.name, evaluator)
			}
			name = phase.name + "/" + name
			l.names[evaluator] = name
			*conditions = recordingConditions{conditions: *conditions, evaluator: evaluator, name: name, log: l, cached: *cache != nil}
			if *cache != nil {
				*cache = recordingCache{EvaluatorCache: *cache, evaluator: evaluator, name: name, log: l}
			}
			byPriority[priority] = append(byPriority[priority], evaluator)
		}
		for _, group := range byPriority {
			groups = append(groups, group)
		}
	}
	return groups
}

type pipelineLogSink struct{ log *pipelineLog }

func (s pipelineLogSink) Init(logr.RuntimeInfo)                 {}
func (s pipelineLogSink) Enabled(int) bool                      { return true }
func (s pipelineLogSink) WithValues(...any) logr.LogSink        { return s }
func (s pipelineLogSink) WithName(string) logr.LogSink          { return s }
func (s pipelineLogSink) Error(err error, msg string, _ ...any) { s.log.failf("%s: %v", msg, err) }

// Info reports the outcome lines of auth_pipeline.go.
// https://github.com/Kuadrant/authorino/blob/v0.26.1/pkg/service/auth_pipeline.go#L280-L380
func (s pipelineLogSink) Info(_ int, msg string, keysAndValues ...any) {
	name, reason := logConfigName(keysAndValues), logValue(keysAndValues, "reason")
	switch msg {
	case "identity validated":
		s.log.authenticated(name)
	case "skipping config":
		if evaluator, ok := logValue(keysAndValues, "config").(auth.AuthConfigEvaluator); ok {
			s.log.decide(evaluator)
		}
	case "cannot fetch metadata":
		if !s.log.failingMetadata[name] {
			s.log.failf("metadata %s: %v", name, reason)
		}
	case "access denied":
		// A rule that doesn't allow the request fails with "Unauthorized". Any other
		// reason is an evaluation error.
		if err, ok := reason.(error); !ok || err.Error() != "Unauthorized" {
			s.log.failf("authorization %s: %v", name, reason)
		} else {
			s.log.denied(name)
		}
	case "cannot build dynamic response":
		s.log.failf("%s %s: %v", msg, name, reason)
	}
}

func logValue(keysAndValues []any, key string) any {
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		if keysAndValues[i] == key {
			return keysAndValues[i+1]
		}
	}
	return nil
}

func logConfigName(keysAndValues []any) string {
	if named, ok := logValue(keysAndValues, "config").(interface{ GetName() string }); ok {
		return named.GetName()
	}
	return fmt.Sprint(logValue(keysAndValues, "config"))
}

// gatewayRequest is a request as it reaches Authorino. identity is the identity of the
// replaced authentication methods (OIDC, TokenReview) and is nil for API keys, which
// authenticate as configured.
type gatewayRequest struct {
	path     string
	headers  map[string]string
	identity map[string]any
	// authMethod is the authentication method that should authenticate the request. A
	// replaced method reads identity from its own header.
	authMethod string
}

// gatewayOutcome is what the gateway produces for one request.
type gatewayOutcome struct {
	allowed bool
	status  envoytype.StatusCode
	body    string
	// headers are the response headers by lower-case name: the upstream headers when
	// the request is allowed, the deny headers when it isn't.
	headers map[string]string
	// metadata is the dynamic metadata built from the success filters, by filter name.
	// TokenRateLimitPolicy predicates read the `identity` filter as auth.identity.
	metadata     map[string]any
	maasAPICalls []maasAPICall
	failures     []string
	// authenticatedBy is the authentication method Authorino kept, empty when none
	// validated the request. deniedBy is the authorization rule whose denial Authorino
	// reported; with several denying rules at one priority, which one is chance.
	authenticatedBy string
	deniedBy        string
}

func (o gatewayOutcome) calledEndpoints() []string {
	endpoints := make([]string, 0, len(o.maasAPICalls))
	for _, call := range o.maasAPICalls {
		endpoints = append(endpoints, call.endpoint)
	}
	return endpoints
}

func (o gatewayOutcome) callBody(endpoint string) any {
	for _, call := range o.maasAPICalls {
		if call.endpoint == endpoint {
			return call.body
		}
	}
	return nil
}

type authorinoGateway struct {
	config  evaluators.AuthConfig
	groups  [][]auth.AuthConfigEvaluator
	maasAPI *fakeMaaSAPI
	log     *pipelineLog
	replies maasAPIReplies
	used    bool
}

// newAuthorinoGateway loads the rules of a generated gateway AuthPolicy into Authorino,
// with maas-api answering from replies. Every CEL expression and predicate in the policy
// must also compile on RHCL's Authorino. spec is left unchanged.
func newAuthorinoGateway(t *testing.T, spec map[string]any, replies maasAPIReplies) *authorinoGateway {
	t.Helper()
	assertAllAuthPolicyCEL_RHCLCompatible(t, spec)
	log := &pipelineLog{
		failingMetadata: map[string]bool{},
		names:           map[auth.AuthConfigEvaluator]string{},
		decided:         map[auth.AuthConfigEvaluator]bool{},
	}
	maasAPI := &fakeMaaSAPI{log: log, replies: replies}
	server := httptest.NewServer(maasAPI)
	t.Cleanup(server.Close)

	authConfigSpec := decodeAuthConfigSpec(t, requiredMap(spec, "defaults", "rules"))
	if len(authConfigSpec.Callbacks) > 0 {
		t.Fatalf("the harness doesn't check callbacks")
	}
	for name, metadata := range authConfigSpec.Metadata {
		// Authorino prefers urlExpression over url, which would bypass the fake.
		if metadata.Http == nil || metadata.Http.Url == "" || metadata.Http.UrlExpression != "" {
			t.Fatalf("metadata %s: the harness supports http metadata with a static url only", name)
		}
		target, err := url.Parse(metadata.Http.Url)
		if err != nil {
			t.Fatalf("metadata %s URL: %v", name, err)
		}
		metadata.Http.Url = server.URL + "/" + target.Scheme + "/" + target.Host + target.EscapedPath()
		if target.RawQuery != "" {
			metadata.Http.Url += "?" + target.RawQuery
		}
		if _, unreachable := replies[target.Path].(unreachableReply); unreachable {
			log.failingMetadata[name] = true
		}
	}
	for name, method := range authConfigSpec.Authentication {
		if method.Plain != nil {
			continue
		}
		method.AuthenticationMethodSpec = authorinoapi.AuthenticationMethodSpec{
			Plain: &authorinoapi.PlainIdentitySpec{Selector: "request.headers." + testIdentityHeader + "-" + name + "|@fromstr"},
		}
		method.Credentials = authorinoapi.Credentials{}
		authConfigSpec.Authentication[name] = method
	}
	authConfigSpec.Hosts = []string{gatewayTestHost}
	authConfig := &authorinoapi.AuthConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "gateway-ns"},
		Spec:       authConfigSpec,
	}

	scheme := runtime.NewScheme()
	if err := authorinoapi.AddToScheme(scheme); err != nil {
		t.Fatalf("add Authorino types to scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(authConfig).WithStatusSubresource(authConfig).Build()
	authConfigIndex := index.NewIndex()
	reconciler := &authorinocontrollers.AuthConfigReconciler{
		Client:       k8sClient,
		Logger:       logr.Discard(),
		Index:        authConfigIndex,
		StatusReport: authorinocontrollers.NewStatusReportMap(),
	}
	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(authConfig)}); err != nil {
		t.Fatalf("Authorino rejected the AuthConfig: %v", err)
	}
	translated := authConfigIndex.Get(gatewayTestHost)
	if translated == nil {
		t.Fatalf("Authorino did not index the AuthConfig: %v", reconciler.StatusReport)
	}
	groups := log.instrument(t, translated)
	return &authorinoGateway{config: *translated, groups: groups, maasAPI: maasAPI, log: log, replies: replies}
}

// decodeAuthConfigSpec decodes a copy of the AuthPolicy rules as an Authorino
// AuthConfig spec. The rules are Authorino types, except that Kuadrant's AuthPolicy
// calls the success dynamicMetadata `filters`, which Kuadrant renames when it writes the
// AuthConfig.
// https://github.com/Kuadrant/kuadrant-operator/blob/v1.4.2/api/v1/authpolicy_types.go#L469
// https://github.com/Kuadrant/kuadrant-operator/blob/v1.4.2/internal/controller/authconfigs_reconciler.go#L223-L247
func decodeAuthConfigSpec(t *testing.T, rules map[string]any) authorinoapi.AuthConfigSpec {
	t.Helper()
	var copied map[string]any
	if err := json.Unmarshal([]byte(mustJSON(rules)), &copied); err != nil {
		t.Fatalf("copy rules: %v", err)
	}
	if response, ok := copied["response"].(map[string]any); ok {
		if success, ok := response["success"].(map[string]any); ok {
			if filters, ok := success["filters"]; ok {
				success["dynamicMetadata"] = filters
				delete(success, "filters")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(mustJSON(copied))))
	decoder.DisallowUnknownFields()
	var authConfigSpec authorinoapi.AuthConfigSpec
	if err := decoder.Decode(&authConfigSpec); err != nil {
		t.Fatalf("rules are not an Authorino AuthConfig spec: %v", err)
	}
	return authConfigSpec
}

// evaluate sends the request through the gateway and fails tb on every failure the
// harness reports for it. A gateway serves one request: Authorino's metadata cache would
// answer a second one without calling maas-api.
func (g *authorinoGateway) evaluate(tb testing.TB, request gatewayRequest) gatewayOutcome {
	tb.Helper()
	outcome := g.evaluateUnchecked(tb, request)
	for _, failure := range outcome.failures {
		tb.Errorf("Authorino: %s", failure)
	}
	return outcome
}

// evaluateUnchecked is evaluate without failing tb, for tests of the harness itself.
func (g *authorinoGateway) evaluateUnchecked(tb testing.TB, request gatewayRequest) gatewayOutcome {
	tb.Helper()
	if g.used {
		tb.Fatalf("a gateway serves one request; create another for %s", request.path)
	}
	g.used = true
	// Envoy's header map always carries the pseudo-headers, and the wasm-shim forwards the
	// whole map; it refuses to build a CheckRequest from an empty one.
	// https://github.com/Kuadrant/wasm-shim/blob/v0.12.1/src/kuadrant/resolver/wasm_host.rs#L32-L36
	headers := map[string]string{":authority": gatewayTestHost, ":method": http.MethodPost, ":path": request.path, ":scheme": "https"}
	maps.Copy(headers, request.headers)
	if request.identity != nil {
		if request.authMethod == "" {
			tb.Fatalf("request for %s has an identity but no authMethod to read it", request.path)
		}
		headers[testIdentityHeader+"-"+request.authMethod] = mustJSON(request.identity)
	}
	check := &envoyauth.CheckRequest{Attributes: &envoyauth.AttributeContext{
		Request: &envoyauth.AttributeContext_Request{Http: &envoyauth.AttributeContext_HttpRequest{
			Host:    gatewayTestHost,
			Path:    request.path,
			Method:  http.MethodPost,
			Headers: headers,
		}},
	}}
	ctx := authorinolog.IntoContext(tb.Context(), logr.New(pipelineLogSink{log: g.log}))
	result := authorinoservice.NewAuthPipeline(ctx, check, g.config).Evaluate()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		pending := g.log.undecided(g.groups)
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatalf("Authorino evaluators %v still undecided 5s after the request", pending)
		}
	}

	outcome := gatewayOutcome{
		allowed:      result.Code == rpc.OK,
		status:       result.Status,
		body:         result.Body,
		headers:      map[string]string{},
		metadata:     result.Metadata,
		maasAPICalls: g.maasAPI.recordedCalls(),
		failures:     g.log.recordedFailures(),
	}
	// Authorino keeps the first identity that validates and reports the first denial,
	// then stops the phase. More than one of either would mean that has changed.
	authenticated, denied := g.log.recordedDecisions()
	switch len(authenticated) {
	case 0:
	case 1:
		outcome.authenticatedBy = authenticated[0]
	default:
		outcome.failures = append(outcome.failures, fmt.Sprintf("identity validated by %v, want one", authenticated))
	}
	switch len(denied) {
	case 0:
	case 1:
		outcome.deniedBy = denied[0]
	default:
		outcome.failures = append(outcome.failures, fmt.Sprintf("denial reported by %v, want one", denied))
	}
	for _, group := range result.Headers {
		for name, value := range group {
			outcome.headers[strings.ToLower(name)] = value
		}
	}
	for endpoint := range g.replies {
		if !slices.Contains(outcome.calledEndpoints(), endpoint) {
			outcome.failures = append(outcome.failures, fmt.Sprintf("case supplies a reply for %s, which the policy never calls", endpoint))
		}
	}
	return outcome
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// apiKeyRequest is a request authenticated with a Bearer API key.
func apiKeyRequest(path string) gatewayRequest {
	return gatewayRequest{path: path, headers: map[string]string{"authorization": "Bearer sk-oai-key"}, authMethod: "api-keys"}
}

// xAPIKeyRequest is a request authenticated with the x-api-key header.
func xAPIKeyRequest(path string) gatewayRequest {
	return gatewayRequest{path: path, headers: map[string]string{"x-api-key": "sk-oai-key"}, authMethod: "api-keys-x-api-key"}
}

func tokenReviewRequest(path string, groups []string) gatewayRequest {
	user := map[string]any{"username": "system:serviceaccount:ns:sa"}
	if len(groups) > 0 {
		user["groups"] = groups
	}
	return gatewayRequest{
		path:       path,
		headers:    map[string]string{"authorization": "Bearer sha256~token", "x-maas-subscription": "gold"},
		identity:   map[string]any{"authenticated": true, "user": user, "audiences": []any{"https://kubernetes.default.svc"}},
		authMethod: "openshift-identities",
	}
}

func oidcRequest(path string, claims map[string]any) gatewayRequest {
	return gatewayRequest{
		path:       path,
		headers:    map[string]string{"authorization": "Bearer aaa.bbb.ccc"},
		identity:   claims,
		authMethod: "oidc-identities",
	}
}

func TestCompileForGatewayRejectsNewerCELFunctions(t *testing.T) {
	if err := compileForGateway(`"abc".reverse()`, nil); err != nil {
		t.Errorf("strings extension v3 function: %v", err)
	}
	// first() comes with optional types v2, which cel-go v0.21.0 lacks.
	if err := compileForGateway(`["a", "b"].first()`, nil); err == nil {
		t.Error("first() compiles, want an error as on the gateway")
	}
	// cel-go v0.21.0 has no backtick-quoted identifiers, only backticks inside strings.
	if err := compileForGateway("request.headers.`x-api-key` == \"k\"", cel.BoolType); err == nil {
		t.Error("a backtick-quoted field compiles, want a syntax error as on the gateway")
	}
	if err := compileForGateway(`request.path == "a`+"`"+`b"`, cel.BoolType); err != nil {
		t.Errorf("a backtick inside a string literal: %v", err)
	}
	if err := compileForGateway(`request.headers[?"x-api-key"].orValue("") == "k"`, cel.BoolType); err != nil {
		t.Errorf("optional index syntax: %v", err)
	}
}

func TestAuthorinoGatewayReportsFailures(t *testing.T) {
	// The harness replaces the method with a plain identity, as it does OIDC and TokenReview.
	identityAuthentication := map[string]any{"test-identity": map[string]any{"anonymous": map[string]any{}}}
	unauthorized := map[string]any{"body": map[string]any{"value": "denied"}}
	request := gatewayRequest{path: "/v1/me", identity: map[string]any{"sub": "alice"}, authMethod: "test-identity"}
	metadataCall := func(rawURL, bodyExpression string) map[string]any {
		return map[string]any{"http": map[string]any{
			"url":         rawURL,
			"method":      "POST",
			"contentType": "application/json",
			"body":        map[string]any{"expression": bodyExpression},
		}}
	}

	t.Run("failing success header and stray reply", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"response": map[string]any{
				"success": map[string]any{
					"headers": map[string]any{
						"broken-header": map[string]any{"plain": map[string]any{"expression": `auth.identity.missing`}},
					},
				},
				"unauthorized": unauthorized,
			},
		}}}
		gateway := newAuthorinoGateway(t, spec, maasAPIReplies{"/unused": map[string]any{}})

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		assertReported(t, outcome.failures, "cannot build dynamic response broken-header", "case supplies a reply for /unused")
	})

	t.Run("failing authorization predicate", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"authorization": map[string]any{
				"broken-rule": map[string]any{"patternMatching": map[string]any{
					"patterns": []any{map[string]any{"predicate": `auth.identity.missing == "x"`}},
				}},
			},
			"response": map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, nil)

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		assertReported(t, outcome.failures, "authorization broken-rule")
	})

	t.Run("failing metadata request", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"metadata": map[string]any{
				"broken-metadata": metadataCall("https://maas-api.ns.svc.cluster.local:8443/internal/v1/lookup", `auth.identity.missing`),
			},
			"response": map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, maasAPIReplies{"/internal/v1/lookup": map[string]any{}})

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		assertReported(t, outcome.failures, "metadata broken-metadata", "case supplies a reply for /internal/v1/lookup")
	})

	t.Run("failing when", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"response": map[string]any{
				"success": map[string]any{
					"headers": map[string]any{
						"conditional-header": map[string]any{
							"when":  []any{map[string]any{"predicate": `auth.identity.missing == "x"`}},
							"plain": map[string]any{"value": "x"},
						},
					},
				},
				"unauthorized": unauthorized,
			},
		}}}
		gateway := newAuthorinoGateway(t, spec, nil)

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		assertReported(t, outcome.failures, "when response/conditional-header")
	})

	t.Run("maas-api call records the host the policy calls", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"metadata": map[string]any{
				"lookup": metadataCall("https://maas-api-x.ns.svc.cluster.local:8443/internal/v1/lookup", `auth.identity`),
			},
			"response": map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, maasAPIReplies{"/internal/v1/lookup": map[string]any{}})

		// when
		outcome := gateway.evaluate(t, request)

		// then
		want := []maasAPICall{{scheme: "https", host: "maas-api-x.ns.svc.cluster.local:8443", endpoint: "/internal/v1/lookup", body: map[string]any{"sub": "alice"}}}
		if !reflect.DeepEqual(outcome.maasAPICalls, want) {
			t.Errorf("maas-api calls = %#v, want %#v", outcome.maasAPICalls, want)
		}
	})

	t.Run("failing cache key", func(t *testing.T) {
		// given
		lookup := metadataCall("https://maas-api.ns.svc.cluster.local:8443/internal/v1/lookup", `auth.identity`)
		lookup["cache"] = map[string]any{"key": map[string]any{"expression": `auth.identity.missing`}, "ttl": 60}
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"metadata":       map[string]any{"lookup": lookup},
			"response":       map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, maasAPIReplies{"/internal/v1/lookup": map[string]any{}})

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		assertReported(t, outcome.failures, "cache key metadata/lookup")
	})

	t.Run("cache key resolved after the phase is decided", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"authorization": map[string]any{
				"deny": map[string]any{"patternMatching": map[string]any{
					"patterns": []any{map[string]any{"predicate": `false`}},
				}},
				"cached": map[string]any{
					"patternMatching": map[string]any{"patterns": []any{map[string]any{"predicate": `true`}}},
					"cache":           map[string]any{"key": map[string]any{"expression": `auth.identity.missing`}, "ttl": 60},
				},
			},
			"response": map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, nil)
		// deny can't decide the phase before cached's `when` has run, so Authorino can't
		// cancel cached, and cached resolves its key well after deny decided.
		cachedWhenRan := make(chan struct{})
		for _, evaluator := range gateway.config.AuthorizationConfigs {
			config, ok := evaluator.(*evaluators.AuthorizationConfig)
			if !ok {
				t.Fatalf("authorization evaluator %T", evaluator)
			}
			if config.Name == "deny" {
				config.Conditions = gatedConditions{Expression: config.Conditions, wait: cachedWhenRan}
				continue
			}
			config.Conditions = signallingConditions{Expression: config.Conditions, ran: cachedWhenRan}
			recording, ok := config.Cache.(recordingCache)
			if !ok {
				t.Fatalf("cache of %s is %T, want the harness's recordingCache", config.Name, config.Cache)
			}
			recording.EvaluatorCache = slowCache{EvaluatorCache: recording.EvaluatorCache, delay: 100 * time.Millisecond}
			config.Cache = recording
		}

		// when
		outcome := gateway.evaluateUnchecked(t, request)

		// then
		if outcome.allowed {
			t.Errorf("request allowed, want deny to decide")
		}
		assertReported(t, outcome.failures, "cache key authorization/cached")
	})

	t.Run("evaluate fails the test on a reported failure", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"response": map[string]any{"success": map[string]any{"headers": map[string]any{
				"broken-header": map[string]any{"plain": map[string]any{"expression": `auth.identity.missing`}},
			}}},
		}}}
		gateway := newAuthorinoGateway(t, spec, nil)
		recorder := &recordingTB{TB: t}

		// when
		gateway.evaluate(recorder, request)

		// then
		assertReported(t, recorder.errors, "Authorino: cannot build dynamic response broken-header")
	})

	t.Run("spec is left unchanged and needs no response", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": map[string]any{
				"oidc": map[string]any{
					"jwt":         map[string]any{"issuerUrl": "https://issuer.example.com"},
					"credentials": map[string]any{"authorizationHeader": map[string]any{"prefix": "Bearer"}},
				},
			},
			"metadata": map[string]any{
				"lookup": metadataCall("https://maas-api.ns.svc.cluster.local:8443/internal/v1/lookup", `auth.identity`),
			},
		}}}
		before := mustJSON(spec)

		// when
		gateway := newAuthorinoGateway(t, spec, maasAPIReplies{"/internal/v1/lookup": map[string]any{}})
		req := request
		req.authMethod = "oidc"
		outcome := gateway.evaluate(t, req)

		// then
		if after := mustJSON(spec); after != before {
			t.Errorf("spec changed:\n%s\nwant\n%s", after, before)
		}
		if !outcome.allowed || outcome.authenticatedBy != "oidc" {
			t.Errorf("allowed = %v (%v) by %q, want the replaced oidc method to authenticate it", outcome.allowed, outcome.status, outcome.authenticatedBy)
		}
	})
}

// gatedConditions holds a `when` until another evaluator's `when` has run.
type gatedConditions struct {
	jsonexp.Expression
	wait <-chan struct{}
}

func (c gatedConditions) Matches(authJSON string) (bool, error) {
	<-c.wait
	return c.Expression.Matches(authJSON)
}

// signallingConditions closes ran once its `when` has run.
type signallingConditions struct {
	jsonexp.Expression
	ran chan struct{}
}

func (c signallingConditions) Matches(authJSON string) (bool, error) {
	defer close(c.ran)
	return c.Expression.Matches(authJSON)
}

// slowCache resolves its key after a delay.
type slowCache struct {
	evaluators.EvaluatorCache
	delay time.Duration
}

func (c slowCache) ResolveKeyFor(authJSON string) (any, error) {
	time.Sleep(c.delay)
	return c.EvaluatorCache.ResolveKeyFor(authJSON)
}

// recordingTB records Errorf calls instead of failing the test. evaluate calls Errorf
// on the test goroutine only.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func assertReported(t *testing.T, failures []string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !slices.ContainsFunc(failures, func(failure string) bool { return strings.HasPrefix(failure, want) }) {
			t.Errorf("failures %v do not report %q", failures, want)
		}
	}
}
