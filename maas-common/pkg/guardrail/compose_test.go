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
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	"k8s.io/apimachinery/pkg/types"
)

const (
	targetNS = "tenant-ns"
	modelNS  = "model-ns"
	modelA   = "model-a"
	modelB   = "model-b"
)

func acceptedProjection(name string, checks ...aiguardrail.Check) *aiguardrail.Policy {
	return &aiguardrail.Policy{
		Namespace:       targetNS,
		Name:            name,
		UID:             types.UID("uid-" + name),
		Generation:      1,
		BindingRevision: "rev-" + name,
		Conditions: []aiguardrail.PolicyCondition{{
			Type:               aiguardrail.ConditionAccepted,
			Status:             true,
			ObservedGeneration: 1,
		}},
		Checks: checks,
	}
}

type policyReader struct {
	policies map[string]*aiguardrail.Policy
}

func (r *policyReader) GetPolicy(_ context.Context, _, name string) (*aiguardrail.Policy, error) {
	policy, ok := r.policies[name]
	if !ok {
		return nil, aiguardrail.ErrPolicyNotFound
	}
	return policy, nil
}

func composeReader() *policyReader {
	return &policyReader{policies: map[string]*aiguardrail.Policy{
		"tenant-pol":  acceptedProjection("tenant-pol", newCheck("ta", 0, phaseIn)),
		"sub-pol":     acceptedProjection("sub-pol", newCheck("sa", 0, phaseIn)),
		"model-pol-a": acceptedProjection("model-pol-a", newCheck("ma", 0, phaseIn)),
		"model-pol-b": acceptedProjection("model-pol-b", newCheck("mb", 0, phaseIn)),
	}}
}

type namespaceRecordingReader struct {
	delegate   aiguardrail.PolicyReader
	namespaces []string
}

func (r *namespaceRecordingReader) GetPolicy(ctx context.Context, namespace, name string) (*aiguardrail.Policy, error) {
	r.namespaces = append(r.namespaces, namespace)
	return r.delegate.GetPolicy(ctx, namespace, name)
}

func tenantWith(attachments ...string) *TenantSelection {
	return &TenantSelection{
		ResourceRef: ResourceRef{Namespace: targetNS, Name: "default", UID: "uid-tenant"},
		Guardrails:  attachmentsFor(attachments),
	}
}

func tenantConfigWith(attachments ...string) *TenantConfigSelection {
	return &TenantConfigSelection{
		ResourceRef: ResourceRef{Namespace: targetNS, Name: "default-tenant", UID: "uid-tenant-config"},
		Guardrails:  attachmentsFor(attachments),
	}
}

func modelWith(namespace, name string, attachments ...string) *ModelSelection {
	return &ModelSelection{
		ResourceRef: ResourceRef{Namespace: namespace, Name: name, UID: types.UID("uid-" + name)},
		Guardrails:  attachmentsFor(attachments),
	}
}

func subscriptionWith(global []string, entries map[string][]string) *SubscriptionSelection {
	refs := make([]ModelEntrySelection, 0, len(entries))
	for name, atts := range entries {
		refs = append(refs, ModelEntrySelection{
			Namespace:  modelNS,
			Name:       name,
			Guardrails: attachmentsFor(atts),
		})
	}
	return &SubscriptionSelection{
		ResourceRef: ResourceRef{Namespace: targetNS, Name: "gold", UID: "uid-gold"},
		Guardrails:  attachmentsFor(global),
		ModelRefs:   refs,
	}
}

func attachmentsFor(names []string) []aiguardrail.Attachment {
	out := make([]aiguardrail.Attachment, 0, len(names))
	for _, n := range names {
		out = append(out, aiguardrail.Attachment{
			Name: n,
		})
	}
	return out
}

func TestCompose_MergesTenantSubscriptionAndModelSelections(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith("tenant-pol")
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{
		modelA: {"model-pol-a"},
	})

	key, plan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantKey := SubscriptionModelKey{SubscriptionNamespace: targetNS, SubscriptionName: "gold", ModelNamespace: modelNS, ModelName: modelA}
	if key != wantKey {
		t.Errorf("key = %+v, want %+v", key, wantKey)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"ma", "sa", "ta"}) {
		t.Errorf("checks = %v, want [ma sa ta]", got)
	}
	wantSources := []Source{
		{Kind: "MaaSSubscription", Namespace: targetNS, Name: "gold", UID: "uid-gold", AttachmentPath: "MaaSSubscription/gold.spec.modelRefs[model-ns/model-a].guardrails[0]"},
		{Kind: "MaaSSubscription", Namespace: targetNS, Name: "gold", UID: "uid-gold", AttachmentPath: "MaaSSubscription/gold.spec.guardrails[0]"},
		{Kind: "AITenant", Namespace: targetNS, Name: "default", UID: "uid-tenant", AttachmentPath: "AITenant/default.spec.guardrails[0]"},
	}
	for i, want := range wantSources {
		if got := plan.Checks[i].Sources; !reflect.DeepEqual(got, []Source{want}) {
			t.Errorf("check %q sources = %+v, want %+v", plan.Checks[i].Check.Name, got, want)
		}
	}
}

func TestCompose_DeduplicatesCheckAndPreservesAllSelectionSources(t *testing.T) {
	reader := composeReader()
	reader.policies["privacy"] = acceptedProjection("privacy", newCheck("pii", 0, phaseIn))
	resolver := aiguardrail.NewAttachmentResolver(reader)
	tenant := tenantWith("privacy")
	tenantConfig := tenantConfigWith("privacy")
	sub := subscriptionWith([]string{"privacy"}, map[string][]string{
		modelA: {"privacy"},
	})
	model := modelWith(modelNS, modelA, "privacy")

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), resolver, targetNS, tenant, tenantConfig, sub, model, modelNS, modelA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"pii"}) {
		t.Fatalf("checks = %v, want [pii]", got)
	}

	wantSources := []Source{
		{Kind: "AITenant", Namespace: targetNS, Name: "default", UID: "uid-tenant", AttachmentPath: "AITenant/default.spec.guardrails[0]"},
		{Kind: "MaaSModelRef", Namespace: modelNS, Name: modelA, UID: "uid-" + modelA, AttachmentPath: "MaaSModelRef/model-a.spec.guardrails[0]"},
		{Kind: "MaaSSubscription", Namespace: targetNS, Name: "gold", UID: "uid-gold", AttachmentPath: "MaaSSubscription/gold.spec.guardrails[0]"},
		{Kind: "MaaSSubscription", Namespace: targetNS, Name: "gold", UID: "uid-gold", AttachmentPath: "MaaSSubscription/gold.spec.modelRefs[model-ns/model-a].guardrails[0]"},
		{Kind: "MaasTenantConfig", Namespace: targetNS, Name: "default-tenant", UID: "uid-tenant-config", AttachmentPath: "MaasTenantConfig/default-tenant.spec.guardrails[0]"},
	}
	if got := plan.Checks[0].Sources; !reflect.DeepEqual(got, wantSources) {
		t.Errorf("sources = %+v, want %+v", got, wantSources)
	}
}

func TestCompose_MergesTenantConfigAndModelScopes(t *testing.T) {
	reader := composeReader()
	reader.policies["config-pol"] = acceptedProjection("config-pol", newCheck("config", 0, phaseIn))
	reader.policies["model-resource-pol"] = acceptedProjection("model-resource-pol", newCheck("resource", 0, phaseIn))
	resolver := aiguardrail.NewAttachmentResolver(reader)

	_, plan, err := ComposeSubscriptionModelPlan(
		context.Background(), resolver, targetNS,
		tenantWith("tenant-pol"), tenantConfigWith("config-pol"),
		subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}}),
		modelWith(modelNS, modelA, "model-resource-pol"), modelNS, modelA,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := checkNames(plan), []string{"config", "ma", "resource", "sa", "ta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("checks = %v, want %v", got, want)
	}
	for _, selected := range plan.Checks {
		if selected.Check.Name == "config" {
			want := Source{Kind: "MaasTenantConfig", Namespace: targetNS, Name: "default-tenant", UID: "uid-tenant-config", AttachmentPath: "MaasTenantConfig/default-tenant.spec.guardrails[0]"}
			if !reflect.DeepEqual(selected.Sources, []Source{want}) {
				t.Errorf("tenant config source = %+v, want %+v", selected.Sources, want)
			}
		}
		if selected.Check.Name == "resource" {
			want := Source{Kind: "MaaSModelRef", Namespace: modelNS, Name: modelA, UID: "uid-" + modelA, AttachmentPath: "MaaSModelRef/model-a.spec.guardrails[0]"}
			if !reflect.DeepEqual(selected.Sources, []Source{want}) {
				t.Errorf("model source = %+v, want %+v", selected.Sources, want)
			}
		}
	}
}

func TestCompose_UsesTargetNamespaceForTenantConfigAndModelNamespaceForModel(t *testing.T) {
	reader := &namespaceRecordingReader{delegate: composeReader()}
	resolver := aiguardrail.NewAttachmentResolver(reader)
	tenantConfig := tenantConfigWith("sub-pol")
	model := modelWith("models-ns", modelA, "model-pol-a")
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})
	sub.ModelRefs[0].Namespace = model.Namespace

	_, _, err := ComposeSubscriptionModelPlan(
		context.Background(), resolver, targetNS,
		tenantWith("tenant-pol"), tenantConfig, sub, model, model.Namespace, modelA,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{targetNS, targetNS, targetNS, targetNS, "models-ns"}
	if !reflect.DeepEqual(reader.namespaces, want) {
		t.Errorf("resolved namespaces = %v, want %v", reader.namespaces, want)
	}
}

func TestCompose_RejectsTenantConfigOutsideTargetNamespace(t *testing.T) {
	reader := &namespaceRecordingReader{delegate: composeReader()}
	resolver := aiguardrail.NewAttachmentResolver(reader)
	tenantConfig := tenantConfigWith("sub-pol")
	tenantConfig.Namespace = "other-tenant-ns"
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})

	_, plan, err := ComposeSubscriptionModelPlan(
		context.Background(), resolver, targetNS, tenantWith("tenant-pol"), tenantConfig, sub,
		modelWith(modelNS, modelA), modelNS, modelA,
	)
	if !errors.Is(err, ErrInvalidCompositionInput) {
		t.Fatalf("got %v, want ErrInvalidCompositionInput", err)
	}
	if plan != nil {
		t.Errorf("want nil plan, got %+v", plan)
	}
	if len(reader.namespaces) != 0 {
		t.Errorf("resolver called for namespaces %v, want no calls", reader.namespaces)
	}
}

func TestCompose_RejectsSubscriptionOutsideTargetNamespace(t *testing.T) {
	reader := &namespaceRecordingReader{delegate: composeReader()}
	resolver := aiguardrail.NewAttachmentResolver(reader)
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})
	sub.Namespace = "other-tenant-ns"

	_, plan, err := ComposeSubscriptionModelPlan(
		context.Background(), resolver, targetNS, tenantWith(), tenantConfigWith(), sub,
		modelWith(modelNS, modelA), modelNS, modelA,
	)
	if !errors.Is(err, ErrInvalidCompositionInput) {
		t.Fatalf("got %v, want ErrInvalidCompositionInput", err)
	}
	if plan != nil {
		t.Errorf("want nil plan, got %+v", plan)
	}
	if len(reader.namespaces) != 0 {
		t.Errorf("resolver called for namespaces %v, want no calls", reader.namespaces)
	}
}

func TestCompose_PreservesAttachmentCheckSelection(t *testing.T) {
	reader := composeReader()
	reader.policies["tenant-pol"] = acceptedProjection("tenant-pol",
		newCheck("first", 0, phaseIn),
		newCheck("second", 1, phaseOut),
	)
	resolver := aiguardrail.NewAttachmentResolver(reader)
	tenant := tenantWith("tenant-pol")
	tenant.Guardrails[0].Checks = []string{"second"}
	sub := subscriptionWith(nil, map[string][]string{modelA: nil})

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), resolver, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := checkNames(plan); !reflect.DeepEqual(got, []string{"second"}) {
		t.Errorf("checks = %v, want [second]", got)
	}
}

func TestCompose_UsesValidatedTargetNamespace(t *testing.T) {
	reader := &namespaceRecordingReader{delegate: composeReader()}
	resolver := aiguardrail.NewAttachmentResolver(reader)
	tenant := tenantWith("tenant-pol")
	tenant.Namespace = "infra-ns"
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), resolver, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, namespace := range reader.namespaces {
		if namespace != targetNS {
			t.Errorf("resolved policy in namespace %q, want %q", namespace, targetNS)
		}
	}

	for _, selected := range plan.Checks {
		if selected.Policy.Name != "tenant-pol" {
			continue
		}
		if got := selected.Sources[0].Namespace; got != "infra-ns" {
			t.Errorf("AITenant provenance namespace = %q, want infra-ns", got)
		}
		return
	}
	t.Fatal("tenant baseline check not found")
}

func TestCompose_DifferentModelsDifferentPlans(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith("tenant-pol")
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{
		modelA: {"model-pol-a"},
		modelB: {"model-pol-b"},
	})

	_, planA, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("model A: %v", err)
	}
	_, planB, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelB), modelNS, modelB)
	if err != nil {
		t.Fatalf("model B: %v", err)
	}

	if got := checkNames(planA); !reflect.DeepEqual(got, []string{"ma", "sa", "ta"}) {
		t.Errorf("planA = %v, want [ma sa ta]", got)
	}
	if got := checkNames(planB); !reflect.DeepEqual(got, []string{"mb", "sa", "ta"}) {
		t.Errorf("planB = %v, want [mb sa ta]", got)
	}
}

func TestCompose_SameModelDifferentSubscriptions(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith("tenant-pol")
	gold := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})
	silver := subscriptionWith([]string{"model-pol-b"}, map[string][]string{modelA: {"model-pol-a"}})
	silver.Name = "silver"
	silver.UID = "uid-silver"

	goldKey, goldPlan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), gold, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("gold: %v", err)
	}
	silverKey, silverPlan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), silver, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("silver: %v", err)
	}
	if goldKey == silverKey {
		t.Errorf("subscription-specific keys are equal: %+v", goldKey)
	}
	if got := checkNames(goldPlan); !reflect.DeepEqual(got, []string{"ma", "sa", "ta"}) {
		t.Errorf("gold plan = %v, want [ma sa ta]", got)
	}
	if got := checkNames(silverPlan); !reflect.DeepEqual(got, []string{"ma", "mb", "ta"}) {
		t.Errorf("silver plan = %v, want [ma mb ta]", got)
	}
}

func TestCompose_RequiresInputs(t *testing.T) {
	resolver := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith()
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})

	for _, tc := range []struct {
		name         string
		resolver     *aiguardrail.AttachmentResolver
		tenant       *TenantSelection
		tenantConfig *TenantConfigSelection
		sub          *SubscriptionSelection
		model        *ModelSelection
	}{
		{name: "resolver", tenant: tenant, tenantConfig: tenantConfigWith(), sub: sub, model: modelWith(modelNS, modelA)},
		{name: "tenant", resolver: resolver, tenantConfig: tenantConfigWith(), sub: sub, model: modelWith(modelNS, modelA)},
		{name: "tenant config", resolver: resolver, tenant: tenant, sub: sub, model: modelWith(modelNS, modelA)},
		{name: "subscription", resolver: resolver, tenant: tenant, tenantConfig: tenantConfigWith(), model: modelWith(modelNS, modelA)},
		{name: "model", resolver: resolver, tenant: tenant, tenantConfig: tenantConfigWith(), sub: sub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, plan, err := ComposeSubscriptionModelPlan(
				context.Background(), tc.resolver, targetNS, tc.tenant, tc.tenantConfig, tc.sub, tc.model, modelNS, modelA,
			)
			if !errors.Is(err, ErrInvalidCompositionInput) {
				t.Fatalf("got %v, want ErrInvalidCompositionInput", err)
			}
			if plan != nil {
				t.Errorf("want nil plan, got %+v", plan)
			}
		})
	}
}

func TestCompose_RejectsMismatchedModelSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *ModelSelection
	}{
		{name: "name", model: modelWith(modelNS, modelB)},
		{name: "namespace", model: modelWith("other-model-ns", modelA)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &namespaceRecordingReader{delegate: composeReader()}
			resolver := aiguardrail.NewAttachmentResolver(reader)
			sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})

			_, plan, err := ComposeSubscriptionModelPlan(
				context.Background(), resolver, targetNS, tenantWith(), tenantConfigWith(), sub,
				tc.model, modelNS, modelA,
			)
			if !errors.Is(err, ErrInvalidCompositionInput) {
				t.Fatalf("got %v, want ErrInvalidCompositionInput", err)
			}
			if plan != nil {
				t.Errorf("want nil plan, got %+v", plan)
			}
			if len(reader.namespaces) != 0 {
				t.Errorf("resolver called for namespaces %v, want no calls", reader.namespaces)
			}
		})
	}
}

func TestCompose_EmptyPlan(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith()
	sub := subscriptionWith(nil, map[string][]string{modelA: nil})

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Checks) != 0 {
		t.Errorf("want empty plan, got %v", checkNames(plan))
	}
}

func TestCompose_MissingModelEntry(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith()
	sub := subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}})

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, "missing"), modelNS, "missing")
	if !errors.Is(err, ErrModelEntryNotFound) {
		t.Fatalf("got %v, want ErrModelEntryNotFound", err)
	}
	if plan != nil {
		t.Errorf("want nil plan, got %+v", plan)
	}
}

func TestCompose_AmbiguousModelEntry(t *testing.T) {
	r := aiguardrail.NewAttachmentResolver(composeReader())
	tenant := tenantWith()
	sub := &SubscriptionSelection{
		ResourceRef: ResourceRef{Namespace: targetNS, Name: "gold", UID: "uid-gold"},
		ModelRefs: []ModelEntrySelection{
			{Namespace: modelNS, Name: modelA, Guardrails: attachmentsFor([]string{"model-pol-a"})},
			{Namespace: modelNS, Name: modelA, Guardrails: attachmentsFor([]string{"model-pol-b"})},
		},
	}

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if !errors.Is(err, ErrModelEntryAmbiguous) {
		t.Fatalf("got %v, want ErrModelEntryAmbiguous", err)
	}
	if plan != nil {
		t.Errorf("want nil plan, got %+v", plan)
	}
}

func TestCompose_WrapsResolutionFailuresWithKeyAndAttachmentPath(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tenant       *TenantSelection
		tenantConfig *TenantConfigSelection
		sub          *SubscriptionSelection
		model        *ModelSelection
		wantPath     string
	}{
		{
			name:         "tenant baseline",
			tenant:       tenantWith("ghost"),
			tenantConfig: tenantConfigWith(),
			sub:          subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}}),
			model:        modelWith(modelNS, modelA),
			wantPath:     "AITenant/default.spec.guardrails[0]",
		},
		{
			name:         "tenant config",
			tenant:       tenantWith(),
			tenantConfig: tenantConfigWith("ghost"),
			sub:          subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}}),
			model:        modelWith(modelNS, modelA),
			wantPath:     "MaasTenantConfig/default-tenant.spec.guardrails[0]",
		},
		{
			name:         "subscription global",
			tenant:       tenantWith(),
			tenantConfig: tenantConfigWith(),
			sub:          subscriptionWith([]string{"sub-pol", "ghost"}, map[string][]string{modelA: {"model-pol-a"}}),
			model:        modelWith(modelNS, modelA),
			wantPath:     "MaaSSubscription/gold.spec.guardrails[1]",
		},
		{
			name:         "model entry",
			tenant:       tenantWith(),
			tenantConfig: tenantConfigWith(),
			sub:          subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"ghost"}}),
			model:        modelWith(modelNS, modelA),
			wantPath:     "MaaSSubscription/gold.spec.modelRefs[model-ns/model-a].guardrails[0]",
		},
		{
			name:         "model resource",
			tenant:       tenantWith(),
			tenantConfig: tenantConfigWith(),
			sub:          subscriptionWith([]string{"sub-pol"}, map[string][]string{modelA: {"model-pol-a"}}),
			model:        modelWith(modelNS, modelA, "ghost"),
			wantPath:     "MaaSModelRef/model-a.spec.guardrails[0]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := aiguardrail.NewAttachmentResolver(composeReader())
			_, plan, err := ComposeSubscriptionModelPlan(
				context.Background(), resolver, targetNS, tc.tenant, tc.tenantConfig, tc.sub, tc.model, modelNS, modelA,
			)
			if !errors.Is(err, aiguardrail.ErrPolicyNotFound) {
				t.Fatalf("got %v, want ErrPolicyNotFound", err)
			}
			for _, want := range []string{
				"compose " + (SubscriptionModelKey{SubscriptionNamespace: targetNS, SubscriptionName: "gold", ModelNamespace: modelNS, ModelName: modelA}).String(),
				tc.wantPath,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if plan != nil {
				t.Errorf("want nil plan on failure, got %+v", plan)
			}
		})
	}
}

// generationBumpReader returns the same policy name with an incrementing
// generation on each read, simulating a policy changing between two reads.
type generationBumpReader struct {
	base  *aiguardrail.Policy
	calls int
}

func (g *generationBumpReader) GetPolicy(_ context.Context, _, _ string) (*aiguardrail.Policy, error) {
	g.calls++
	p := *g.base
	p.Generation = int64(g.calls)
	p.Conditions = []aiguardrail.PolicyCondition{{
		Type:               aiguardrail.ConditionAccepted,
		Status:             true,
		ObservedGeneration: int64(g.calls),
	}}
	return &p, nil
}

func TestCompose_SnapshotConflictAcrossScopes(t *testing.T) {
	reader := &generationBumpReader{base: acceptedProjection("shared", newCheck("a", 0, phaseIn))}
	r := aiguardrail.NewAttachmentResolver(reader)
	tenant := tenantWith("shared")
	sub := subscriptionWith([]string{"shared"}, map[string][]string{modelA: nil})

	_, plan, err := ComposeSubscriptionModelPlan(context.Background(), r, targetNS, tenant, tenantConfigWith(), sub, modelWith(modelNS, modelA), modelNS, modelA)
	if !errors.Is(err, ErrPolicySnapshotConflict) {
		t.Fatalf("got %v, want ErrPolicySnapshotConflict", err)
	}
	if plan != nil {
		t.Errorf("want nil plan on conflict, got %+v", plan)
	}
}
