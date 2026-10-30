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
	"fmt"
	"slices"
	"sort"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	"k8s.io/apimachinery/pkg/types"
)

var (
	// ErrPolicySnapshotConflict indicates that selections of the same policy
	// disagree on UID, generation, or binding revision.
	ErrPolicySnapshotConflict = errors.New("guardrail policy snapshot conflict")
	// ErrInvalidCheckMetadata indicates that resolved check metadata cannot form
	// one valid policy snapshot.
	ErrInvalidCheckMetadata = errors.New("invalid guardrail check metadata")
)

// PolicyIdentity identifies the exact AIGuardrail snapshot used by a selection.
type PolicyIdentity struct {
	Namespace       string
	Name            string
	UID             types.UID
	Generation      int64
	BindingRevision string
}

func identityOf(p aiguardrail.ResolvedPolicy) PolicyIdentity {
	return PolicyIdentity{
		Namespace:       p.Namespace,
		Name:            p.Name,
		UID:             p.UID,
		Generation:      p.Generation,
		BindingRevision: p.BindingRevision,
	}
}

// Source identifies where a selection came from. The merge core treats it as
// opaque provenance and does not interpret MaaS scope semantics.
type Source struct {
	Kind           string
	Namespace      string
	Name           string
	UID            types.UID
	AttachmentPath string
}

// ResolvedSelection is one resolved attachment and its origin.
type ResolvedSelection struct {
	Policy aiguardrail.ResolvedPolicy
	Source Source
}

// SelectedCheck is a deduplicated check with its policy identity and every
// source that selected it.
type SelectedCheck struct {
	Policy  PolicyIdentity
	Check   aiguardrail.Check
	Sources []Source
}

// ExecutionPlan is the deterministically ordered list of effective checks. Checks is the
// single source of order; a phase's order is this list filtered to that phase.
type ExecutionPlan struct {
	Checks []SelectedCheck
}

// Merge combines resolved selections into a deterministic execution plan.
// Selection is additive: no source can remove checks contributed by another.
// Checks are deduplicated by (policy namespace, policy name, check name).
// Selections must contain values produced by the AI Gateway AttachmentResolver.
// Merge validates cross-selection consistency and check metadata, but does not repeat policy acceptance or binding validation.
// It fails closed: selections of the same policy must agree on identity, and a duplicated check must agree on its metadata.
// On any conflict it returns nil and an error, never a partial plan. Empty input yields an empty plan.
func Merge(selections []ResolvedSelection) (*ExecutionPlan, error) {
	if err := checkSnapshotConsistency(selections); err != nil {
		return nil, err
	}

	type checkKey struct {
		namespace string
		name      string
		check     string
	}
	index := make(map[checkKey]*SelectedCheck)
	type specIndexKey struct {
		namespace string
		name      string
		index     int
	}
	indexOwners := make(map[specIndexKey]string)

	for _, sel := range selections {
		identity := identityOf(sel.Policy)
		for _, c := range sel.Policy.Checks {
			phases, err := validateAndSortPhases(c.Phases)
			if err != nil {
				return nil, fmt.Errorf("check %q in policy %s/%s has invalid phases: %w: %w",
					c.Name, sel.Policy.Namespace, sel.Policy.Name, err, ErrInvalidCheckMetadata)
			}
			c.Phases = phases
			if c.SpecIndex < 0 {
				return nil, fmt.Errorf("check %q in policy %s/%s has negative spec index %d: %w",
					c.Name, sel.Policy.Namespace, sel.Policy.Name, c.SpecIndex, ErrInvalidCheckMetadata)
			}
			ownerKey := specIndexKey{namespace: sel.Policy.Namespace, name: sel.Policy.Name, index: c.SpecIndex}
			if owner, ok := indexOwners[ownerKey]; ok && owner != c.Name {
				return nil, fmt.Errorf("checks %q and %q in policy %s/%s share spec index %d: %w",
					owner, c.Name, sel.Policy.Namespace, sel.Policy.Name, c.SpecIndex, ErrInvalidCheckMetadata)
			}
			indexOwners[ownerKey] = c.Name

			key := checkKey{namespace: sel.Policy.Namespace, name: sel.Policy.Name, check: c.Name}
			existing, ok := index[key]
			if !ok {
				index[key] = &SelectedCheck{
					Policy:  identity,
					Check:   c,
					Sources: []Source{sel.Source},
				}
				continue
			}
			if existing.Check.SpecIndex != c.SpecIndex || !slices.Equal(existing.Check.Phases, c.Phases) {
				return nil, fmt.Errorf("check %q in policy %s/%s has conflicting metadata: %w",
					c.Name, sel.Policy.Namespace, sel.Policy.Name, ErrInvalidCheckMetadata)
			}
			existing.Sources = append(existing.Sources, sel.Source)
		}
	}

	checks := make([]SelectedCheck, 0, len(index))
	for _, sc := range index {
		sc.Sources = dedupeSources(sc.Sources)
		checks = append(checks, *sc)
	}
	sortChecks(checks)

	return &ExecutionPlan{Checks: checks}, nil
}

// checkSnapshotConsistency enforces that all selections of the same policy
// (namespace+name) share the same UID, generation, and binding revision.
func checkSnapshotConsistency(selections []ResolvedSelection) error {
	type policyKey struct {
		namespace string
		name      string
	}
	seen := make(map[policyKey]PolicyIdentity)
	for _, sel := range selections {
		key := policyKey{namespace: sel.Policy.Namespace, name: sel.Policy.Name}
		identity := identityOf(sel.Policy)
		prev, ok := seen[key]
		if !ok {
			seen[key] = identity
			continue
		}
		if prev != identity {
			return fmt.Errorf("policy %s/%s selected with conflicting snapshots %+v and %+v: %w",
				sel.Policy.Namespace, sel.Policy.Name, prev, identity, ErrPolicySnapshotConflict)
		}
	}
	return nil
}

// sortChecks orders by policy name, then policy namespace, then spec index.
func sortChecks(checks []SelectedCheck) {
	sort.SliceStable(checks, func(i, j int) bool {
		a, b := checks[i], checks[j]
		if a.Policy.Name != b.Policy.Name {
			return a.Policy.Name < b.Policy.Name
		}
		if a.Policy.Namespace != b.Policy.Namespace {
			return a.Policy.Namespace < b.Policy.Namespace
		}
		if a.Check.SpecIndex != b.Check.SpecIndex {
			return a.Check.SpecIndex < b.Check.SpecIndex
		}
		return false
	})
}

// ChecksForPhase returns the plan's checks that run in phase, in plan order.
func (p *ExecutionPlan) ChecksForPhase(phase aigatewayv1alpha1.GuardrailPhase) []SelectedCheck {
	out := make([]SelectedCheck, 0, len(p.Checks))
	for _, sc := range p.Checks {
		if slices.Contains(sc.Check.Phases, phase) {
			out = append(out, cloneSelectedCheck(sc))
		}
	}
	return out
}

// validateAndSortPhases validates phases and returns a sorted copy.
func validateAndSortPhases(phases []aigatewayv1alpha1.GuardrailPhase) ([]aigatewayv1alpha1.GuardrailPhase, error) {
	if len(phases) == 0 {
		return nil, errors.New("phases must not be empty")
	}
	for _, phase := range phases {
		switch phase {
		case aigatewayv1alpha1.GuardrailPhaseInput, aigatewayv1alpha1.GuardrailPhaseOutput:
		default:
			return nil, fmt.Errorf("unknown phase %q", phase)
		}
	}
	out := append([]aigatewayv1alpha1.GuardrailPhase(nil), phases...)
	slices.Sort(out)
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("duplicate phase %q", out[i])
		}
	}
	return out, nil
}

func cloneSelectedCheck(sc SelectedCheck) SelectedCheck {
	sc.Check.Phases = append([]aigatewayv1alpha1.GuardrailPhase(nil), sc.Check.Phases...)
	sc.Sources = append([]Source(nil), sc.Sources...)
	return sc
}

// dedupeSources removes identical sources and sorts them deterministically.
func dedupeSources(sources []Source) []Source {
	seen := make(map[Source]struct{}, len(sources))
	out := make([]Source, 0, len(sources))
	for _, s := range sources {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return sourceLess(out[i], out[j])
	})
	return out
}

func sourceLess(a, b Source) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.UID != b.UID {
		return a.UID < b.UID
	}
	return a.AttachmentPath < b.AttachmentPath
}
