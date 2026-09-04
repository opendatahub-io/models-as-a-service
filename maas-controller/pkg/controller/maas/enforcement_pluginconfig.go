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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Compiles the ext_proc pluginConfig from a gateway's routes and
// subscription intent. Cross-artifact hashes need only be internally consistent,
// not equal to Kuadrant's.

// PluginConfiguration is the pluginConfig ext_proc consumes.
type PluginConfiguration struct {
	Services   map[string]Service `json:"services"`
	ActionSets []ActionSet        `json:"actionSets"`
}

// Service is one entry in the pluginConfig services block.
type Service struct {
	Endpoint    string `json:"endpoint"`
	FailureMode string `json:"failureMode"`
	Timeout     string `json:"timeout"`
	Type        string `json:"type"`
}

// ActionSet is one route's match conditions plus its ordered actions.
type ActionSet struct {
	Name                string              `json:"name"`
	RouteRuleConditions RouteRuleConditions `json:"routeRuleConditions"`
	Actions             []Action            `json:"actions"`
}

// RouteRuleConditions selects a request by hostname and CEL predicates.
type RouteRuleConditions struct {
	Hostnames  []string `json:"hostnames"`
	Predicates []string `json:"predicates"`
}

// Action is one policy step (auth, ratelimit-check, ratelimit-report).
type Action struct {
	Service         string            `json:"service"`
	Scope           string            `json:"scope"`
	Sources         []string          `json:"sources,omitempty"`
	Predicates      []string          `json:"predicates,omitempty"`
	ConditionalData []ConditionalData `json:"conditionalData,omitempty"`
}

// ConditionalData is a descriptor set applied when its predicates hold.
type ConditionalData struct {
	Predicates []string   `json:"predicates,omitempty"`
	Data       []DataItem `json:"data"`
}

// DataItem is one descriptor entry.
type DataItem struct {
	Expression Expression `json:"expression"`
}

// Expression is a Kuadrant descriptor expression.
type Expression struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Upstream endpoints the services block maps to; ext_proc's upstreams map
// resolves each to an address.
const (
	authServiceEndpoint      = "kuadrant-auth-service"
	ratelimitServiceEndpoint = "kuadrant-ratelimit-service"
)

// Logical service names the action sets reference; the services block maps each
// to an endpoint that ext_proc's upstreams map resolves to an address.
const (
	authServiceName            = "auth-service"
	ratelimitServiceName       = "ratelimit-service"
	ratelimitCheckServiceName  = "ratelimit-check-service"
	ratelimitReportServiceName = "ratelimit-report-service"
)

// standardServices is the fixed services block MaaS gateways use.
func standardServices() map[string]Service {
	return map[string]Service{
		authServiceName:            {Endpoint: authServiceEndpoint, FailureMode: "deny", Timeout: "200ms", Type: "auth"},
		ratelimitServiceName:       {Endpoint: ratelimitServiceEndpoint, FailureMode: "allow", Timeout: "100ms", Type: "ratelimit"},
		ratelimitCheckServiceName:  {Endpoint: ratelimitServiceEndpoint, FailureMode: "allow", Timeout: "100ms", Type: "ratelimit-check"},
		ratelimitReportServiceName: {Endpoint: ratelimitServiceEndpoint, FailureMode: "allow", Timeout: "100ms", Type: "ratelimit-report"},
	}
}

// RouteMatch is the subset of an HTTPRoute match we compile.
type RouteMatch struct {
	PathType    string // "Exact" or "PathPrefix"
	PathValue   string
	HeaderName  string
	HeaderValue string
}

// routePredicates turns one HTTPRoute match into its CEL predicates. A header
// match becomes the case-insensitive exists() form.
func routePredicates(m RouteMatch) []string {
	var preds []string
	switch m.PathType {
	case "Exact":
		preds = append(preds, fmt.Sprintf("request.url_path == '%s'", m.PathValue))
	case "PathPrefix":
		preds = append(preds, fmt.Sprintf("request.url_path.startsWith('%s')", m.PathValue))
	}
	if m.HeaderName != "" {
		preds = append(preds, fmt.Sprintf(
			"request.headers.exists(h, h.lowerAscii() == '%s' && request.headers[h] == '%s')",
			strings.ToLower(m.HeaderName), m.HeaderValue))
	}
	return preds
}

// ModelActionSetParams carries the identities a model action set is built from.
type ModelActionSetParams struct {
	Hostname        string
	RoutePredicates []string
	AuthScope       string // = the generated AuthConfig host
	AuthSource      string // provenance ref
	RLScope         string // "<routeNamespace>/<routeName>"
	TRLPSource      string // ratelimit policy provenance ref
	SubscriptionKey string // "<subNs>/<subName>@<modelNs>/<modelName>"
	TokenLimitKey   string // descriptor key body
}

// authGatePredicate excludes the maas-api health probe from auth.
const authGatePredicate = `request.path != "/maas-api/health" || request.method != "GET"`

// tokenUsageAddend is the report-phase hits_addend: the response's total tokens.
const tokenUsageAddend = `responseBodyJSON("/usage/total_tokens")`

// RateLimitDescriptor is one counter within a ratelimit action: the Limitador
// token limit (by descriptor key) and the CEL guard it applies under.
type RateLimitDescriptor struct {
	TokenLimitKey string
	WhenPredicate string
}

// RateLimitBinding is the ratelimit half of an action set: the counter domain
// (scope), provenance, and one descriptor per subscription.
type RateLimitBinding struct {
	Scope       string
	Source      string
	Descriptors []RateLimitDescriptor
}

// buildActionSet compiles one route match into its action set: an auth action, a
// ratelimit check (hits_addend 0), and a ratelimit report (hits_addend = tokens).
func buildActionSet(hostname string, routePreds []string, authScope, authSource string, rl RateLimitBinding) ActionSet {
	rlData := func(hitsAddend string) []ConditionalData {
		cds := make([]ConditionalData, 0, len(rl.Descriptors))
		for _, d := range rl.Descriptors {
			cds = append(cds, ConditionalData{
				Predicates: []string{d.WhenPredicate},
				Data: []DataItem{
					{Expression: Expression{Key: "tokenlimit." + d.TokenLimitKey, Value: "1"}},
					{Expression: Expression{Key: "auth.identity.userid", Value: "auth.identity.userid"}},
					{Expression: Expression{Key: "ratelimit.hits_addend", Value: hitsAddend}},
				},
			})
		}
		return cds
	}

	return ActionSet{
		Name:                actionSetName(hostname, routePreds),
		RouteRuleConditions: RouteRuleConditions{Hostnames: []string{hostname}, Predicates: routePreds},
		Actions: []Action{
			{Service: authServiceName, Scope: authScope, Sources: []string{authSource}, Predicates: []string{authGatePredicate}},
			{Service: ratelimitCheckServiceName, Scope: rl.Scope, Sources: []string{rl.Source}, ConditionalData: rlData("0")},
			{Service: ratelimitReportServiceName, Scope: rl.Scope, Sources: []string{rl.Source}, ConditionalData: rlData(tokenUsageAddend)},
		},
	}
}

// modelWhenPredicate guards a subscription's token limit to its own requests,
// excluding the model-listing endpoint.
func modelWhenPredicate(subscriptionKey string) string {
	return fmt.Sprintf(
		`auth.identity.selected_subscription_key == "%s" && !request.path.endsWith("/v1/models")`,
		subscriptionKey)
}

// denyAllWhenPredicate is the gateway safety net: applies to any request that is
// not a maas-api control-plane path.
const denyAllWhenPredicate = `!request.path.startsWith("/maas-api") && ` +
	`!request.path.startsWith("/v1/models") && ` +
	`!request.path.startsWith("/v1/subscriptions") && ` +
	`!request.path.startsWith("/v1/api-keys")`

// buildModelActionSet compiles one model route match for a single subscription.
func buildModelActionSet(p ModelActionSetParams) ActionSet {
	return buildActionSet(p.Hostname, p.RoutePredicates, p.AuthScope, p.AuthSource, RateLimitBinding{
		Scope:  p.RLScope,
		Source: p.TRLPSource,
		Descriptors: []RateLimitDescriptor{
			{TokenLimitKey: p.TokenLimitKey, WhenPredicate: modelWhenPredicate(p.SubscriptionKey)},
		},
	})
}

// buildRouteActionSets compiles every match of one route into an action set,
// sharing the same auth and ratelimit policy.
func buildRouteActionSets(matches []RouteMatch, base ModelActionSetParams) []ActionSet {
	out := make([]ActionSet, 0, len(matches))
	for _, m := range matches {
		p := base
		p.RoutePredicates = routePredicates(m)
		out = append(out, buildModelActionSet(p))
	}
	return out
}

// SubscriptionBinding is one subscription's ratelimit identity and budget, shared
// across the pluginConfig action set and the Limitador limit.
type SubscriptionBinding struct {
	SubscriptionKey string // empty for deny-all
	RouteScope      string // "<routeNs>/<routeName>"
	Source          string
	TokenLimitKey   string
	LimitName       string
	MaxValue        int64 // 0 = deny-all
	Window          string
}

// descriptor turns a subscription binding into its ratelimit descriptor.
func (b SubscriptionBinding) descriptor(denyAll bool) RateLimitDescriptor {
	when := modelWhenPredicate(b.SubscriptionKey)
	if denyAll {
		when = denyAllWhenPredicate
	}
	return RateLimitDescriptor{TokenLimitKey: b.TokenLimitKey, WhenPredicate: when}
}

// GatewayEnforcementInput is the cluster state a gateway's artifacts are
// generated from: the contract between input gathering and the pure generators.
type GatewayEnforcementInput struct {
	Hostname        string
	AuthConfigHost  string // the single AuthConfig host == the pluginConfig auth scope
	AuthSource      string
	AuthRules       map[string]any // from buildGatewayAuthPolicySpec
	ModelMatches    []RouteMatch
	ModelRouteScope string // the model route's RLS domain, shared by its subscriptions
	ModelSource     string
	Subscriptions   []SubscriptionBinding
	MaasAPIMatches  []RouteMatch
	DenyAll         SubscriptionBinding

	// EnforcedSubscriptions get marked Active after their config is applied.
	EnforcedSubscriptions []types.NamespacedName
}

// GatewayArtifacts are the three resources MaaS emits per gateway to enforce
// natively: the extproc pluginConfig, the AuthConfig, and the Limitador limits.
type GatewayArtifacts struct {
	PluginConfig PluginConfiguration
	AuthConfig   map[string]any
	Limits       []LimitadorLimit
}

// generateGatewayArtifacts composes the three artifacts from one input. Pure: the
// reconciler gathers the input and applies the output.
func generateGatewayArtifacts(in GatewayEnforcementInput) (GatewayArtifacts, error) {
	modelRL := RateLimitBinding{Scope: in.ModelRouteScope, Source: in.ModelSource}
	limits := make([]LimitadorLimit, 0, len(in.Subscriptions)+1)
	for _, s := range in.Subscriptions {
		modelRL.Descriptors = append(modelRL.Descriptors, s.descriptor(false))
		l, err := buildLimitadorLimit(s.LimitName, in.ModelRouteScope, s.TokenLimitKey, s.MaxValue, s.Window)
		if err != nil {
			return GatewayArtifacts{}, fmt.Errorf("token limit %s: %w", s.LimitName, err)
		}
		limits = append(limits, l)
	}

	denyRL := RateLimitBinding{
		Scope:       in.DenyAll.RouteScope,
		Source:      in.DenyAll.Source,
		Descriptors: []RateLimitDescriptor{in.DenyAll.descriptor(true)},
	}

	pc := assembleGatewayPluginConfig(GatewayPluginParams{
		Hostname:       in.Hostname,
		AuthScope:      in.AuthConfigHost,
		AuthSource:     in.AuthSource,
		ModelMatches:   in.ModelMatches,
		ModelRL:        modelRL,
		MaasAPIMatches: in.MaasAPIMatches,
		DenyAllRL:      denyRL,
	})

	ac := buildAuthConfigSpec(in.AuthConfigHost, in.AuthRules)

	denyLimit, err := buildLimitadorLimit(in.DenyAll.LimitName, in.DenyAll.RouteScope, in.DenyAll.TokenLimitKey, in.DenyAll.MaxValue, in.DenyAll.Window)
	if err != nil {
		return GatewayArtifacts{}, fmt.Errorf("deny-all limit: %w", err)
	}
	limits = append(limits, denyLimit)

	return GatewayArtifacts{PluginConfig: pc, AuthConfig: ac, Limits: limits}, nil
}

// ownedScopes is the set of Limitador RLS domains this gateway's artifacts own.
func (a GatewayArtifacts) ownedScopes() map[string]bool {
	scopes := map[string]bool{}
	for _, l := range a.Limits {
		scopes[l.Namespace] = true
	}
	return scopes
}

// GatewayPluginParams is everything needed to assemble a gateway's pluginConfig.
type GatewayPluginParams struct {
	Hostname       string
	AuthScope      string
	AuthSource     string
	ModelMatches   []RouteMatch
	ModelRL        RateLimitBinding
	MaasAPIMatches []RouteMatch
	DenyAllRL      RateLimitBinding
}

// assembleGatewayPluginConfig compiles a gateway's model and maas-api routes into
// the full pluginConfig: the fixed services block plus one action set per match.
func assembleGatewayPluginConfig(p GatewayPluginParams) PluginConfiguration {
	sets := make([]ActionSet, 0, len(p.ModelMatches)+len(p.MaasAPIMatches))
	for _, m := range p.ModelMatches {
		sets = append(sets, buildActionSet(p.Hostname, routePredicates(m), p.AuthScope, p.AuthSource, p.ModelRL))
	}
	for _, m := range p.MaasAPIMatches {
		sets = append(sets, buildActionSet(p.Hostname, routePredicates(m), p.AuthScope, p.AuthSource, p.DenyAllRL))
	}
	return PluginConfiguration{Services: standardServices(), ActionSets: sets}
}

// actionSetCondKey is a stable identity for an action set's route conditions,
// used to diff generated action sets against Kuadrant's output.
func actionSetCondKey(a ActionSet) string {
	return strings.Join(a.RouteRuleConditions.Predicates, "|")
}

// actionSetRLKey summarizes an action set's ratelimit binding for diffing.
func actionSetRLKey(a ActionSet) string {
	if len(a.Actions) < 3 || len(a.Actions[2].ConditionalData) == 0 || len(a.Actions[2].ConditionalData[0].Data) == 0 {
		if len(a.Actions) > 1 {
			return a.Actions[1].Scope
		}
		return ""
	}
	return a.Actions[1].Scope + " " + a.Actions[2].ConditionalData[0].Data[0].Expression.Key
}

// actionSetName hashes the hostname and predicates into a stable per-match id.
func actionSetName(hostname string, predicates []string) string {
	h := sha256.Sum256([]byte(hostname + "\n" + strings.Join(predicates, "\n")))
	return hex.EncodeToString(h[:])
}
