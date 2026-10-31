package subscription

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
)

func TestFindModelRef(t *testing.T) {
	sub := &subscription{
		Name: "test-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "claude-opus-4-8", Namespace: "llm", ModelName: "claude-opus-4-8"},
			{Name: "llama-external", Namespace: "llm", ModelName: "meta-llama/llama-3-70b"},
			{Name: "plain-model", Namespace: "other"},
		},
	}

	tests := []struct {
		name           string
		requestedModel string
		wantRefName    string // "" means expect nil
	}{
		{"namespace/name form", "llm/claude-opus-4-8", "claude-opus-4-8"},
		{"raw spec.modelName (body-routed)", "claude-opus-4-8", "claude-opus-4-8"},
		{"raw CRD name fallback", "plain-model", "plain-model"},
		{"raw model name containing slash", "meta-llama/llama-3-70b", "llama-external"},
		{"no match", "unknown-model", ""},
		{"namespace/name shaped but unknown", "llm/unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := findModelRef(sub, tt.requestedModel)
			if tt.wantRefName == "" {
				if ref != nil {
					t.Fatalf("findModelRef(%q) = %q, want nil", tt.requestedModel, ref.Name)
				}
				return
			}
			if ref == nil {
				t.Fatalf("findModelRef(%q) = nil, want ref %q", tt.requestedModel, tt.wantRefName)
			}
			if ref.Name != tt.wantRefName {
				t.Fatalf("findModelRef(%q) = %q, want %q", tt.requestedModel, ref.Name, tt.wantRefName)
			}
		})
	}
}

func TestCheckModelHealthBodyRoutedAlias(t *testing.T) {
	// Degraded subscription whose model carries rate limits: the TRLP check
	// must resolve body-routed raw model names to the canonical ref instead
	// of rejecting them as InvalidModelFormat.
	newDegradedSub := func(trlpReady bool) *subscription {
		return &subscription{
			Name:  "degraded-sub",
			Phase: PhaseDegraded,
			ModelRefs: []ModelRefInfo{{
				Name:            "claude-model",
				Namespace:       "llm",
				ModelName:       "claude-opus-4-8",
				TokenRateLimits: []TokenRateLimit{{Limit: 1000, Window: "1m"}},
			}},
			TokenRateLimitStatuses: []TokenRateLimitStatus{{Model: "claude-model", Ready: trlpReady}},
		}
	}

	tests := []struct {
		name           string
		requestedModel string
		trlpReady      bool
		wantReason     string // "" means expect nil error
	}{
		{"alias with ready TRLP allowed", "claude-opus-4-8", true, ""},
		{"namespace/name with ready TRLP allowed", "llm/claude-model", true, ""},
		{"alias with unready TRLP fails closed", "claude-opus-4-8", false, "RateLimitNotEnforced"},
		{"model not in subscription", "unknown-model", true, "InvalidModelFormat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkModelHealth(newDegradedSub(tt.trlpReady), tt.requestedModel)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("checkModelHealth(%q) = %v, want nil", tt.requestedModel, err)
				}
				return
			}
			var unhealthy *ModelUnhealthyError
			if !errors.As(err, &unhealthy) {
				t.Fatalf("checkModelHealth(%q) = %v, want ModelUnhealthyError", tt.requestedModel, err)
			}
			if unhealthy.Reason != tt.wantReason {
				t.Fatalf("checkModelHealth(%q) reason = %q, want %q", tt.requestedModel, unhealthy.Reason, tt.wantReason)
			}
		})
	}
}

// A path-routed request carries the client-facing model name as the pair's
// name segment. The pair must also match modelRefs whose name differs from
// that client-facing name: renamed models, and per-tenant modelRefs serving
// one shared model.
func TestFindModelRefAliasPair(t *testing.T) {
	sub := &subscription{
		Name: "alias-pair-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
			{Name: "modelref-b", Namespace: "tenant-b", ModelName: "client-name"},
			{Name: "client-name", Namespace: "tenant-b", ModelName: "unrelated-identity"},
			{Name: "legacy-ref", Namespace: "tenant-a"},
		},
	}

	tests := []struct {
		name           string
		requestedModel string
		wantRefName    string // "" means expect nil
	}{
		{"pair name matched via alias", "tenant-a/client-name", "modelref-a"},
		{"pair alias match is namespace-scoped", "tenant-c/client-name", ""},
		{"exact pair takes precedence over alias pair", "tenant-b/client-name", "client-name"},
		{"bare client-facing name still matches", "client-name", "modelref-a"},
		{"pair with no exact or alias match", "tenant-a/unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := findModelRef(sub, tt.requestedModel)
			if tt.wantRefName == "" {
				if ref != nil {
					t.Fatalf("findModelRef(%q) = %q, want nil", tt.requestedModel, ref.Name)
				}
				return
			}
			if ref == nil {
				t.Fatalf("findModelRef(%q) = nil, want ref %q", tt.requestedModel, tt.wantRefName)
			}
			if ref.Name != tt.wantRefName {
				t.Fatalf("findModelRef(%q) = %q, want %q", tt.requestedModel, ref.Name, tt.wantRefName)
			}
		})
	}
}

// The resolveModelAlias test doc and cases above cover the rewrite guard; this
// test pins the pair fallback's fail-closed semantics for ambiguity.
func TestFindModelRefAliasPairAmbiguous(t *testing.T) {
	// One client-facing identity claimed by two distinct modelRefs in the
	// same subscription: ambiguous, so the pair fallback fails closed.
	sub := &subscription{
		Name: "ambiguous-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
			{Name: "modelref-c", Namespace: "tenant-a", ModelName: "client-name"},
		},
	}
	if ref := findModelRef(sub, "tenant-a/client-name"); ref != nil {
		t.Fatalf("findModelRef(%q) = %q, want nil for ambiguous alias", "tenant-a/client-name", ref.Name)
	}

	// The same modelRef listed twice is a duplicate entry, not ambiguity.
	dup := &subscription{
		Name: "dup-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
			{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
		},
	}
	if ref := findModelRef(dup, "tenant-a/client-name"); ref == nil || ref.Name != "modelref-a" {
		t.Fatalf("findModelRef(duplicate entries) = %v, want modelref-a", ref)
	}
}

// findModelRef returns the first alias match, so authorization must consider
// only authorized refs: a subscription may list several refs sharing one
// client-facing name (per-tenant modelRefs for a shared model, or distinct
// models with one client-facing name) while the caller's authorized set
// covers only some of them. An unauthorized ref listed first must not deny
// the request when a later ref is authorized.
func TestIsResolvedModelAuthorizedSharedAlias(t *testing.T) {
	makeSub := func(refs ...ModelRefInfo) *subscription {
		return &subscription{Name: "shared-alias-sub", ModelRefs: refs}
	}
	unauthorizedFirst := makeSub(
		ModelRefInfo{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
		ModelRefInfo{Name: "modelref-b", Namespace: "tenant-b", ModelName: "client-name"},
	)
	authorizedFirst := makeSub(
		ModelRefInfo{Name: "modelref-b", Namespace: "tenant-b", ModelName: "client-name"},
		ModelRefInfo{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
	)
	authorized := map[authpolicy.ModelKey]bool{
		{Namespace: "tenant-b", Name: "modelref-b"}: true,
	}

	tests := []struct {
		name           string
		sub            *subscription
		authorizedSet  map[authpolicy.ModelKey]bool
		requestedModel string
		want           bool
	}{
		{"unauthorized ref listed first", unauthorizedFirst, authorized, "client-name", true},
		{"authorized ref listed first", authorizedFirst, authorized, "client-name", true},
		{"pair in the authorized ref's namespace", unauthorizedFirst, authorized, "tenant-b/client-name", true},
		{"pair in the unauthorized ref's namespace", unauthorizedFirst, authorized, "tenant-a/client-name", false},
		{"no authorized carrier denies", unauthorizedFirst, map[authpolicy.ModelKey]bool{}, "client-name", false},
		{"unrestricted nil set denies", unauthorizedFirst, nil, "client-name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isResolvedModelAuthorized(tt.sub, tt.requestedModel, tt.authorizedSet); got != tt.want {
				t.Fatalf("isResolvedModelAuthorized(%q) = %v, want %v", tt.requestedModel, got, tt.want)
			}
		})
	}
}

// The pair fallback's fail-closed ambiguity check considers only authorized
// refs: when one client-facing identity in a namespace is claimed by two
// refs but the caller is authorized for exactly one of them, the request
// resolves to that ref instead of failing closed on the unauthorized twin.
// Genuinely ambiguous authorized pairs still fail closed.
func TestIsResolvedModelAuthorizedAmbiguousAliasSubset(t *testing.T) {
	sub := &subscription{
		Name: "ambiguous-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "modelref-a", Namespace: "tenant-a", ModelName: "client-name"},
			{Name: "modelref-b", Namespace: "tenant-a", ModelName: "client-name"},
		},
	}
	oneAuthorized := map[authpolicy.ModelKey]bool{
		{Namespace: "tenant-a", Name: "modelref-b"}: true,
	}
	if !isResolvedModelAuthorized(sub, "tenant-a/client-name", oneAuthorized) {
		t.Fatal("isResolvedModelAuthorized(tenant-a/client-name) = false, want true when exactly one ambiguous carrier is authorized")
	}
	bothAuthorized := map[authpolicy.ModelKey]bool{
		{Namespace: "tenant-a", Name: "modelref-a"}: true,
		{Namespace: "tenant-a", Name: "modelref-b"}: true,
	}
	if isResolvedModelAuthorized(sub, "tenant-a/client-name", bothAuthorized) {
		t.Fatal("isResolvedModelAuthorized(tenant-a/client-name) = true, want false while both ambiguous carriers are authorized")
	}
}

// An exact namespace/name reference identifies the requested model — the
// same precedence findModelRef documents. When it is unauthorized, an
// authorized alias carrier in the same namespace must not be substituted
// for it.
func TestIsResolvedModelAuthorizedExactRefPrecedence(t *testing.T) {
	sub := &subscription{
		Name: "exact-sub",
		ModelRefs: []ModelRefInfo{
			{Name: "client-name", Namespace: "tenant-b"},
			{Name: "modelref-b", Namespace: "tenant-b", ModelName: "client-name"},
		},
	}
	carrierAuthorized := map[authpolicy.ModelKey]bool{
		{Namespace: "tenant-b", Name: "modelref-b"}: true,
	}
	if isResolvedModelAuthorized(sub, "tenant-b/client-name", carrierAuthorized) {
		t.Fatal("isResolvedModelAuthorized(tenant-b/client-name) = true, want false when the exact reference is unauthorized")
	}
	bothAuthorized := map[authpolicy.ModelKey]bool{
		{Namespace: "tenant-b", Name: "client-name"}: true,
		{Namespace: "tenant-b", Name: "modelref-b"}:  true,
	}
	if !isResolvedModelAuthorized(sub, "tenant-b/client-name", bothAuthorized) {
		t.Fatal("isResolvedModelAuthorized(tenant-b/client-name) = false, want true when the exact reference is authorized")
	}
}

type stubModelRefLister struct {
	items []*unstructured.Unstructured
}

func (s stubModelRefLister) List() ([]*unstructured.Unstructured, error) {
	return s.items, nil
}

func modelRefWithAlias(name, ns, alias string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetName(name)
	u.SetNamespace(ns)
	if alias != "" {
		u.Object["status"] = map[string]any{"resolvedModelAlias": alias}
	}
	return u
}

// resolveModelAlias must only rewrite a bare client-facing name when exactly
// one MaaSModelRef carries it. A shared model has one carrier per tenant in
// the model's namespace, and distinct models may share a client-facing name
// across namespaces; rewriting to an arbitrary carrier would fail every other
// tenant's subscription matching, so the raw name is preserved for
// findModelRef to match against the caller's own subscription.
func TestResolveModelAliasAmbiguity(t *testing.T) {
	tests := []struct {
		name           string
		items          []*unstructured.Unstructured
		requestedModel string
		want           string
	}{
		{
			name: "unique alias rewritten to namespace/name",
			items: []*unstructured.Unstructured{
				modelRefWithAlias("modelref-a", "tenant-a", "client-name"),
				modelRefWithAlias("other-ref", "tenant-b", "other-name"),
			},
			requestedModel: "client-name",
			want:           "tenant-a/modelref-a",
		},
		{
			name: "cross-namespace shared alias left unchanged",
			items: []*unstructured.Unstructured{
				modelRefWithAlias("modelref-a", "tenant-a", "client-name"),
				modelRefWithAlias("modelref-b", "tenant-b", "client-name"),
			},
			requestedModel: "client-name",
			want:           "client-name",
		},
		{
			name: "same-namespace shared alias left unchanged",
			items: []*unstructured.Unstructured{
				modelRefWithAlias("modelref-a", "tenant-a", "client-name"),
				modelRefWithAlias("modelref-b", "tenant-a", "client-name"),
			},
			requestedModel: "client-name",
			want:           "client-name",
		},
		{
			name:           "no alias match left unchanged",
			items:          []*unstructured.Unstructured{modelRefWithAlias("modelref-a", "tenant-a", "client-name")},
			requestedModel: "unknown-name",
			want:           "unknown-name",
		},
		{
			name:           "nil lister left unchanged",
			items:          nil,
			requestedModel: "client-name",
			want:           "client-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Selector{modelLister: stubModelRefLister{items: tt.items}}
			if got := s.resolveModelAlias(tt.requestedModel); got != tt.want {
				t.Fatalf("resolveModelAlias(%q) = %q, want %q", tt.requestedModel, got, tt.want)
			}
		})
	}
}
