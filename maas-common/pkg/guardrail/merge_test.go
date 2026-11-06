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

package guardrail

import (
	"errors"
	"reflect"
	"testing"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	"k8s.io/apimachinery/pkg/types"
)

var (
	phaseIn    = aigatewayv1alpha1.GuardrailPhaseInput
	phaseOut   = aigatewayv1alpha1.GuardrailPhaseOutput
	phaseInOut = []aigatewayv1alpha1.GuardrailPhase{phaseIn, phaseOut}
)

func resolvedPolicy(name string, checks ...aiguardrail.Check) aiguardrail.ResolvedPolicy {
	return aiguardrail.ResolvedPolicy{
		Namespace:       targetNS,
		Name:            name,
		UID:             types.UID("uid-" + name),
		Generation:      1,
		BindingRevision: "rev-" + name,
		Checks:          checks,
	}
}

func newCheck(name string, specIndex int, phases ...aigatewayv1alpha1.GuardrailPhase) aiguardrail.Check {
	return aiguardrail.Check{Name: name, SpecIndex: specIndex, Phases: phases}
}

func newSource(kind, name, path string) Source {
	return Source{Kind: kind, Namespace: targetNS, Name: name, UID: types.UID("uid-" + name), AttachmentPath: path}
}

func checkNames(plan *ExecutionPlan) []string {
	out := make([]string, 0, len(plan.Checks))
	for _, c := range plan.Checks {
		out = append(out, c.Check.Name)
	}
	return out
}

func TestMerge_EmptyInput(t *testing.T) {
	plan, err := Merge(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil || len(plan.Checks) != 0 {
		t.Fatalf("want empty plan, got %+v", plan)
	}
}

func TestMerge_SingleSelection(t *testing.T) {
	sel := ResolvedSelection{
		Policy: resolvedPolicy("alpha", newCheck("a", 0, phaseIn), newCheck("b", 1, phaseOut)),
		Source: newSource("AITenant", "default", "AITenant/default.spec.guardrails[0]"),
	}
	plan, err := Merge([]ResolvedSelection{sel})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := &ExecutionPlan{Checks: []SelectedCheck{
		{Policy: identityOf(sel.Policy), Check: newCheck("a", 0, phaseIn), Sources: []Source{sel.Source}},
		{Policy: identityOf(sel.Policy), Check: newCheck("b", 1, phaseOut), Sources: []Source{sel.Source}},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Errorf("plan mismatch:\n got %+v\nwant %+v", plan, want)
	}
}

func TestMerge_DeduplicatesChecksAndRetainsDistinctSources(t *testing.T) {
	p := resolvedPolicy("alpha", newCheck("a", 0, phaseIn), newCheck("b", 1, phaseOut))
	selections := []ResolvedSelection{
		{Policy: p, Source: newSource("AITenant", "default", "AITenant/default.spec.guardrails[0]")},
		{Policy: p, Source: newSource("MaaSSubscription", "gold", "MaaSSubscription/gold.spec.guardrails[0]")},
	}
	plan, err := Merge(selections)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("checks = %v, want [a b] (deduped)", got)
	}
	for _, sc := range plan.Checks {
		if len(sc.Sources) != 2 {
			t.Errorf("check %q sources = %d, want 2", sc.Check.Name, len(sc.Sources))
		}
		if sc.Sources[0].Kind != "AITenant" || sc.Sources[1].Kind != "MaaSSubscription" {
			t.Errorf("sources not sorted: %+v", sc.Sources)
		}
	}
}

func TestMerge_KeepsSameNamedChecksFromDifferentPolicies(t *testing.T) {
	privacy := resolvedPolicy("privacy", newCheck("pii", 0, phaseIn))
	compliance := resolvedPolicy("compliance", newCheck("pii", 0, phaseIn))
	plan, err := Merge([]ResolvedSelection{
		{Policy: privacy, Source: newSource("AITenant", "default", "p0")},
		{Policy: compliance, Source: newSource("MaaSSubscription", "gold", "p1")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(plan.Checks))
	}
	if got := []string{plan.Checks[0].Policy.Name, plan.Checks[1].Policy.Name}; !reflect.DeepEqual(got, []string{"compliance", "privacy"}) {
		t.Errorf("policy names = %v, want [compliance privacy]", got)
	}
}

func TestMerge_DeduplicatesIdenticalSources(t *testing.T) {
	p := resolvedPolicy("alpha", newCheck("a", 0, phaseIn))
	same := newSource("AITenant", "default", "AITenant/default.spec.guardrails[0]")
	plan, err := Merge([]ResolvedSelection{
		{Policy: p, Source: same},
		{Policy: p, Source: same},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := len(plan.Checks[0].Sources); n != 1 {
		t.Errorf("duplicate source not deduped: %d sources", n)
	}
}

func TestMerge_RejectsSnapshotConflicts(t *testing.T) {
	base := func() aiguardrail.ResolvedPolicy {
		return aiguardrail.ResolvedPolicy{Namespace: targetNS, Name: "alpha", UID: "u", Generation: 1, BindingRevision: "r", Checks: []aiguardrail.Check{newCheck("a", 0, phaseIn)}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(p *aiguardrail.ResolvedPolicy)
	}{
		{name: "uid", mutate: func(p *aiguardrail.ResolvedPolicy) { p.UID = "other" }},
		{name: "generation", mutate: func(p *aiguardrail.ResolvedPolicy) { p.Generation = 9 }},
		{name: "binding revision", mutate: func(p *aiguardrail.ResolvedPolicy) { p.BindingRevision = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			b := base()
			tc.mutate(&b)
			plan, err := Merge([]ResolvedSelection{
				{Policy: a, Source: newSource("AITenant", "default", "p0")},
				{Policy: b, Source: newSource("MaaSSubscription", "gold", "p1")},
			})
			if !errors.Is(err, ErrPolicySnapshotConflict) {
				t.Fatalf("got %v, want ErrPolicySnapshotConflict", err)
			}
			if plan != nil {
				t.Fatalf("want nil plan, got %+v", plan)
			}
		})
	}
}

func TestMerge_RejectsConflictingCheckMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    aiguardrail.Check
		b    aiguardrail.Check
	}{
		{"spec index", newCheck("a", 0, phaseIn), newCheck("a", 1, phaseIn)},
		{"phases", newCheck("a", 0, phaseIn), newCheck("a", 0, phaseOut)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pa := resolvedPolicy("alpha", tc.a)
			pb := resolvedPolicy("alpha", tc.b)
			plan, err := Merge([]ResolvedSelection{
				{Policy: pa, Source: newSource("AITenant", "default", "p0")},
				{Policy: pb, Source: newSource("MaaSSubscription", "gold", "p1")},
			})
			if !errors.Is(err, ErrInvalidCheckMetadata) {
				t.Fatalf("got %v, want ErrInvalidCheckMetadata", err)
			}
			if plan != nil {
				t.Fatalf("want nil plan, got %+v", plan)
			}
		})
	}
}

func TestMerge_DisjointChecksSameSnapshot(t *testing.T) {
	pa := resolvedPolicy("alpha", newCheck("a", 0, phaseIn))
	pb := resolvedPolicy("alpha", newCheck("b", 1, phaseOut))
	plan, err := Merge([]ResolvedSelection{
		{Policy: pa, Source: newSource("AITenant", "default", "p0")},
		{Policy: pb, Source: newSource("MaaSSubscription", "gold", "p1")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("checks = %v, want [a b]", got)
	}
}

func TestMerge_ValidatesAndSortsPhases(t *testing.T) {
	pa := resolvedPolicy("alpha", newCheck("a", 0, phaseOut, phaseIn))
	pb := resolvedPolicy("alpha", newCheck("a", 0, phaseIn, phaseOut))
	plan, err := Merge([]ResolvedSelection{
		{Policy: pa, Source: newSource("AITenant", "default", "p0")},
		{Policy: pb, Source: newSource("MaaSSubscription", "gold", "p1")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Checks) != 1 {
		t.Errorf("want 1 deduped check, got %d", len(plan.Checks))
	}
	if got := plan.Checks[0].Check.Phases; !reflect.DeepEqual(got, phaseInOut) {
		t.Errorf("phases = %v, want %v", got, phaseInOut)
	}
}

func TestMerge_RejectsDuplicateSpecIndex(t *testing.T) {
	plan, err := Merge([]ResolvedSelection{{
		Policy: resolvedPolicy("alpha", newCheck("a", 0, phaseIn), newCheck("b", 0, phaseOut)),
		Source: newSource("AITenant", "default", "p0"),
	}})
	if !errors.Is(err, ErrInvalidCheckMetadata) {
		t.Fatalf("got %v, want ErrInvalidCheckMetadata", err)
	}
	if plan != nil {
		t.Fatalf("want nil plan, got %+v", plan)
	}
}

func TestMerge_RejectsInvalidPhases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phases []aigatewayv1alpha1.GuardrailPhase
	}{
		{name: "empty"},
		{name: "duplicate", phases: []aigatewayv1alpha1.GuardrailPhase{phaseIn, phaseIn}},
		{name: "unknown", phases: []aigatewayv1alpha1.GuardrailPhase{"Transform"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Merge([]ResolvedSelection{{
				Policy: resolvedPolicy("alpha", newCheck("a", 0, tc.phases...)),
				Source: newSource("AITenant", "default", "p0"),
			}})
			if !errors.Is(err, ErrInvalidCheckMetadata) {
				t.Fatalf("got %v, want ErrInvalidCheckMetadata", err)
			}
			if plan != nil {
				t.Fatalf("want nil plan, got %+v", plan)
			}
		})
	}
}

func TestMerge_DoesNotAliasInputPhaseSlice(t *testing.T) {
	phases := []aigatewayv1alpha1.GuardrailPhase{phaseIn}
	plan, err := Merge([]ResolvedSelection{{
		Policy: resolvedPolicy("alpha", newCheck("a", 0, phases...)),
		Source: newSource("AITenant", "default", "p0"),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	phases[0] = phaseOut
	if got := plan.Checks[0].Check.Phases; !reflect.DeepEqual(got, []aigatewayv1alpha1.GuardrailPhase{phaseIn}) {
		t.Errorf("plan phases changed with input: %v", got)
	}
}

func TestMerge_OrderingIsIndependentOfSelectionOrder(t *testing.T) {
	beta := resolvedPolicy("beta", newCheck("z", 0, phaseIn))
	alpha := resolvedPolicy("alpha", newCheck("b", 1, phaseIn), newCheck("a", 0, phaseIn))
	first, err := Merge([]ResolvedSelection{
		{Policy: beta, Source: newSource("MaaSSubscription", "gold", "p0")},
		{Policy: alpha, Source: newSource("AITenant", "default", "p1")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := Merge([]ResolvedSelection{
		{Policy: alpha, Source: newSource("AITenant", "default", "p1")},
		{Policy: beta, Source: newSource("MaaSSubscription", "gold", "p0")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(first); !reflect.DeepEqual(got, []string{"a", "b", "z"}) {
		t.Errorf("checks = %v, want [a b z]", got)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("plans differ by selection order:\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

func TestMerge_UsesNamespaceAsPolicyNameTiebreaker(t *testing.T) {
	a := aiguardrail.ResolvedPolicy{Namespace: "ns-b", Name: "alpha", UID: "u1", Generation: 1, BindingRevision: "r", Checks: []aiguardrail.Check{newCheck("b", 0, phaseIn)}}
	b := aiguardrail.ResolvedPolicy{Namespace: "ns-a", Name: "alpha", UID: "u2", Generation: 1, BindingRevision: "r", Checks: []aiguardrail.Check{newCheck("a", 0, phaseIn)}}
	plan, err := Merge([]ResolvedSelection{
		{Policy: a, Source: newSource("AITenant", "default", "p0")},
		{Policy: b, Source: newSource("MaaSSubscription", "gold", "p1")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("checks = %v, want [a b]", got)
	}
}

func TestMerge_SourceSortingTiebreakers(t *testing.T) {
	p := resolvedPolicy("alpha", newCheck("a", 0, phaseIn))
	base := Source{Kind: "k", Namespace: "ns", Name: "nm", UID: "uid", AttachmentPath: "p"}
	withKind := base
	withKind.Kind = "z"
	withNS := base
	withNS.Namespace = "zz"
	withName := base
	withName.Name = "zz"
	withUID := base
	withUID.UID = "zz"
	withPath := base
	withPath.AttachmentPath = "zz"

	in := []Source{withPath, withKind, base, withUID, withNS, withName}
	selections := make([]ResolvedSelection, 0, len(in))
	for _, s := range in {
		selections = append(selections, ResolvedSelection{Policy: p, Source: s})
	}
	plan, err := Merge(selections)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Source{base, withPath, withUID, withName, withNS, withKind}
	if got := plan.Checks[0].Sources; !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %+v, want %+v", got, want)
	}
}

func TestMerge_PhaseFilteringPreservesOrder(t *testing.T) {
	p := resolvedPolicy("alpha",
		newCheck("a", 0, phaseIn),
		newCheck("b", 1, phaseOut),
		newCheck("c", 2, phaseInOut...),
	)
	plan, err := Merge([]ResolvedSelection{{Policy: p, Source: newSource("AITenant", "default", "p0")}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotIn := make([]string, 0)
	for _, sc := range plan.ChecksForPhase(phaseIn) {
		gotIn = append(gotIn, sc.Check.Name)
	}
	if !reflect.DeepEqual(gotIn, []string{"a", "c"}) {
		t.Errorf("input phase = %v, want [a c]", gotIn)
	}
	gotOut := make([]string, 0)
	for _, sc := range plan.ChecksForPhase(phaseOut) {
		gotOut = append(gotOut, sc.Check.Name)
	}
	if !reflect.DeepEqual(gotOut, []string{"b", "c"}) {
		t.Errorf("output phase = %v, want [b c]", gotOut)
	}
}

func TestExecutionPlan_ChecksForPhaseReturnsIndependentCopies(t *testing.T) {
	p := resolvedPolicy("alpha", newCheck("a", 0, phaseIn))
	plan, err := Merge([]ResolvedSelection{{
		Policy: p,
		Source: newSource("AITenant", "default", "p0"),
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := plan.ChecksForPhase(phaseIn)
	checks[0].Check.Phases[0] = phaseOut
	checks[0].Sources[0].Name = "changed"

	if got := plan.Checks[0].Check.Phases; !reflect.DeepEqual(got, []aigatewayv1alpha1.GuardrailPhase{phaseIn}) {
		t.Errorf("plan phases = %v, want [Input]", got)
	}
	if got := plan.Checks[0].Sources[0].Name; got != "default" {
		t.Errorf("plan source name = %q, want default", got)
	}
}
