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
	"slices"
	"strings"
	"sync"
	"testing"

	envoyauth "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	envoytype "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/gogo/googleapis/google/rpc"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	authorinoapi "github.com/kuadrant/authorino/api/v1beta3"
	authorinocontrollers "github.com/kuadrant/authorino/controllers"
	"github.com/kuadrant/authorino/pkg/evaluators"
	authorinocel "github.com/kuadrant/authorino/pkg/expressions/cel"
	"github.com/kuadrant/authorino/pkg/index"
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
// of its priority group, and a `when` that errors skips its evaluator. The metadata,
// authorization and response failures Authorino only logs come back in
// gatewayOutcome.failures.
//
// Not covered, so these need a test of their own:
//   - Authentication other than API keys. It is replaced by a plain identity read from
//     testIdentityHeader; its `when`, priority, overrides and defaults stay.
//   - Kuadrant's translation of the AuthPolicy into an AuthConfig, apart from the
//     rename of the success filters to dynamicMetadata.
//   - The wasm-shim, including spec.defaults.when.

// gatewayCELLibraries are the CEL library versions RHCL's Authorino v0.24.0 gets from
// cel-go v0.21.0. This module builds with cel-go v0.29.2, so Authorino would pick up
// newer libraries with functions the gateway lacks. cel-go keeps the first registration
// of a library, so passing these to Compile ahead of Authorino's own pins them.
// https://github.com/Kuadrant/authorino/blob/v0.24.0/pkg/expressions/cel/expressions.go#L89-L159
var gatewayCELLibraries = []cel.EnvOption{
	ext.Strings(ext.StringsVersion(3)),
	cel.OptionalTypes(cel.OptionalTypesVersion(1)),
}

func compileForGateway(expression string, outputType *cel.Type) error {
	_, err := authorinocel.Compile(expression, outputType, gatewayCELLibraries...)
	return err
}

const (
	gatewayTestHost = "gateway.test"
	// testIdentityHeader carries the identity for authentication methods the tests
	// replace with a plain identity.
	testIdentityHeader = "x-test-identity"

	validateEndpoint = "/internal/v1/api-keys/validate"
	selectEndpoint   = "/internal/v1/subscriptions/select"
)

// authenticationMethods are the AuthConfig authentication kinds other than plain.
// The tests can't reach an OIDC issuer or a Kubernetes API, so these are replaced by a
// plain identity read from testIdentityHeader. Their `when` and priority stay.
var authenticationMethods = []string{"apiKey", "jwt", "kubernetesTokenReview", "oauth2Introspection", "x509", "anonymous"}

// maasAPIReplies holds maas-api's reply for each endpoint the gateway may call. Calling
// an endpoint without a reply fails the test, and so does a reply for an endpoint the
// policy never calls.
type maasAPIReplies map[string]any

type unreachableReply struct{}

// maasAPIUnreachable as a reply closes the connection, as an unreachable maas-api would.
var maasAPIUnreachable = unreachableReply{}

type maasAPICall struct {
	endpoint string
	body     any
}

// fakeMaaSAPI answers the gateway's maas-api calls with a case's replies and records
// them.
type fakeMaaSAPI struct {
	t       *testing.T
	mu      sync.Mutex
	replies maasAPIReplies
	calls   []maasAPICall
}

func (f *fakeMaaSAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read maas-api request body: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		f.t.Errorf("maas-api %s got a body that is not JSON: %q", r.URL.Path, body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, maasAPICall{endpoint: r.URL.Path, body: decoded})
	reply, ok := f.replies[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("maas-api %s called without a reply in the case", r.URL.Path)
	}
	if _, unreachable := reply.(unreachableReply); !ok || unreachable {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		f.t.Errorf("encode maas-api reply: %v", err)
	}
}

func (f *fakeMaaSAPI) recordedCalls() []maasAPICall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// pipelineLog collects the evaluator failures Authorino reports only in its log: a
// metadata call or a response config that fails, or an authorization rule that fails
// for a reason other than a denial.
type pipelineLog struct {
	mu sync.Mutex
	// failingMetadata names the metadata evaluators whose maas-api is unreachable on purpose.
	failingMetadata map[string]bool
	failures        []string
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

type pipelineLogSink struct{ log *pipelineLog }

func (s pipelineLogSink) Init(logr.RuntimeInfo)                 {}
func (s pipelineLogSink) Enabled(int) bool                      { return true }
func (s pipelineLogSink) WithValues(...any) logr.LogSink        { return s }
func (s pipelineLogSink) WithName(string) logr.LogSink          { return s }
func (s pipelineLogSink) Error(err error, msg string, _ ...any) { s.log.failf("%s: %v", msg, err) }

// Info reports the outcome lines of auth_pipeline.go.
// https://github.com/Kuadrant/authorino/blob/v0.24.0/pkg/service/auth_pipeline.go#L270-L380
func (s pipelineLogSink) Info(_ int, msg string, keysAndValues ...any) {
	name, reason := logConfigName(keysAndValues), logValue(keysAndValues, "reason")
	switch msg {
	case "cannot fetch metadata":
		if !s.log.failingMetadata[name] {
			s.log.failf("metadata %s: %v", name, reason)
		}
	case "access denied":
		// A rule that doesn't allow the request fails with "Unauthorized". Any other
		// reason is an evaluation error.
		if err, ok := reason.(error); !ok || err.Error() != "Unauthorized" {
			s.log.failf("authorization %s: %v", name, reason)
		}
	case "cannot build dynamic response", "cannot execute callback":
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
	maasAPI *fakeMaaSAPI
	log     *pipelineLog
	replies maasAPIReplies
}

// newAuthorinoGateway loads the rules of a generated gateway AuthPolicy into Authorino,
// with maas-api answering from replies.
func newAuthorinoGateway(t *testing.T, spec map[string]any, replies maasAPIReplies) *authorinoGateway {
	t.Helper()
	maasAPI := &fakeMaaSAPI{t: t, replies: replies}
	server := httptest.NewServer(maasAPI)
	t.Cleanup(server.Close)

	rules := requiredMap(spec, "defaults", "rules")
	log := &pipelineLog{failingMetadata: map[string]bool{}}
	for name := range requiredMap(rules, "metadata") {
		httpConfig := requiredMap(rules, "metadata", name, "http")
		target, err := url.Parse(fmt.Sprint(httpConfig["url"]))
		if err != nil {
			t.Fatalf("metadata %s URL: %v", name, err)
		}
		httpConfig["url"] = server.URL + target.Path
		if _, unreachable := replies[target.Path].(unreachableReply); unreachable {
			log.failingMetadata[name] = true
		}
	}
	for name := range requiredMap(rules, "authentication") {
		method := requiredMap(rules, "authentication", name)
		if _, plain := method["plain"]; plain {
			continue
		}
		for _, kind := range authenticationMethods {
			delete(method, kind)
		}
		delete(method, "credentials")
		method["plain"] = map[string]any{"selector": "request.headers." + testIdentityHeader + "|@fromstr"}
	}

	// The rules are Authorino types, except that Kuadrant's AuthPolicy calls the success
	// dynamicMetadata `filters`, which Kuadrant renames when it writes the AuthConfig.
	// https://github.com/Kuadrant/kuadrant-operator/blob/v1.4.2/api/v1/authpolicy_types.go#L469
	// https://github.com/Kuadrant/kuadrant-operator/blob/v1.4.2/internal/controller/authconfigs_reconciler.go#L223-L247
	if success, ok := rules["response"].(map[string]any)["success"].(map[string]any); ok {
		if filters, ok := success["filters"]; ok {
			success["dynamicMetadata"] = filters
			delete(success, "filters")
		}
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("marshal rules: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var authConfigSpec authorinoapi.AuthConfigSpec
	if err := decoder.Decode(&authConfigSpec); err != nil {
		t.Fatalf("rules are not an Authorino AuthConfig spec: %v", err)
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
	return &authorinoGateway{config: *translated, maasAPI: maasAPI, log: log, replies: replies}
}

func (g *authorinoGateway) evaluate(t *testing.T, request gatewayRequest) gatewayOutcome {
	t.Helper()
	headers := maps.Clone(request.headers)
	if request.identity != nil {
		headers[testIdentityHeader] = mustJSON(request.identity)
	}
	check := &envoyauth.CheckRequest{Attributes: &envoyauth.AttributeContext{
		Request: &envoyauth.AttributeContext_Request{Http: &envoyauth.AttributeContext_HttpRequest{
			Host:    gatewayTestHost,
			Path:    request.path,
			Method:  http.MethodPost,
			Headers: headers,
		}},
	}}
	ctx := authorinolog.IntoContext(t.Context(), logr.New(pipelineLogSink{log: g.log}))
	result := authorinoservice.NewAuthPipeline(ctx, check, g.config).Evaluate()

	outcome := gatewayOutcome{
		allowed:      result.Code == rpc.OK,
		status:       result.Status,
		body:         result.Body,
		headers:      map[string]string{},
		metadata:     result.Metadata,
		maasAPICalls: g.maasAPI.recordedCalls(),
		failures:     g.log.recordedFailures(),
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
	return gatewayRequest{path: path, headers: map[string]string{"authorization": "Bearer sk-oai-key"}}
}

// xAPIKeyRequest is a request authenticated with the x-api-key header.
func xAPIKeyRequest(path string) gatewayRequest {
	return gatewayRequest{path: path, headers: map[string]string{"x-api-key": "sk-oai-key"}}
}

func tokenReviewRequest(path string, groups []string) gatewayRequest {
	user := map[string]any{"username": "system:serviceaccount:ns:sa"}
	if len(groups) > 0 {
		user["groups"] = groups
	}
	return gatewayRequest{
		path:     path,
		headers:  map[string]string{"authorization": "Bearer sha256~token", "x-maas-subscription": "gold"},
		identity: map[string]any{"authenticated": true, "user": user, "audiences": []any{"https://kubernetes.default.svc"}},
	}
}

func oidcRequest(path string, claims map[string]any) gatewayRequest {
	return gatewayRequest{
		path:     path,
		headers:  map[string]string{"authorization": "Bearer aaa.bbb.ccc"},
		identity: claims,
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
}

func TestAuthorinoGatewayReportsFailures(t *testing.T) {
	identityAuthentication := map[string]any{
		"test-identity": map[string]any{"plain": map[string]any{"selector": "request.headers." + testIdentityHeader + "|@fromstr"}},
	}
	unauthorized := map[string]any{"body": map[string]any{"value": "denied"}}
	request := gatewayRequest{path: "/v1/me", headers: map[string]string{}, identity: map[string]any{"sub": "alice"}}

	t.Run("failing success header and stray reply", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"metadata":       map[string]any{},
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
		outcome := gateway.evaluate(t, request)

		// then
		assertReported(t, outcome.failures, "cannot build dynamic response broken-header", "case supplies a reply for /unused")
	})

	t.Run("failing authorization predicate", func(t *testing.T) {
		// given
		spec := map[string]any{"defaults": map[string]any{"rules": map[string]any{
			"authentication": identityAuthentication,
			"metadata":       map[string]any{},
			"authorization": map[string]any{
				"broken-rule": map[string]any{"patternMatching": map[string]any{
					"patterns": []any{map[string]any{"predicate": `auth.identity.missing == "x"`}},
				}},
			},
			"response": map[string]any{"unauthorized": unauthorized},
		}}}
		gateway := newAuthorinoGateway(t, spec, nil)

		// when
		outcome := gateway.evaluate(t, request)

		// then
		assertReported(t, outcome.failures, "authorization broken-rule")
	})
}

func assertReported(t *testing.T, failures []string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !slices.ContainsFunc(failures, func(failure string) bool { return strings.HasPrefix(failure, want) }) {
			t.Errorf("failures %v do not report %q", failures, want)
		}
	}
}
