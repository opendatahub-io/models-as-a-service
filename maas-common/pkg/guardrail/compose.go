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
	"fmt"

	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
)

var (
	// ErrInvalidCompositionInput indicates that composition input is missing or inconsistent.
	ErrInvalidCompositionInput = errors.New("invalid guardrail composition input")
	// ErrModelEntryNotFound indicates that the subscription has no modelRefs
	// entry for the requested model.
	ErrModelEntryNotFound = errors.New("subscription model entry not found")
	// ErrModelEntryAmbiguous indicates that the subscription has more than one
	// modelRefs entry for the requested model.
	ErrModelEntryAmbiguous = errors.New("subscription model entry ambiguous")
)

// SubscriptionModelKey identifies a subscription/model pair.
// Effective guardrail selection can differ for the same model under different subscriptions.
type SubscriptionModelKey struct {
	SubscriptionNamespace string
	SubscriptionName      string
	ModelNamespace        string
	ModelName             string
}

func (k SubscriptionModelKey) String() string {
	return fmt.Sprintf("%s/%s@%s/%s", k.SubscriptionNamespace, k.SubscriptionName, k.ModelNamespace, k.ModelName)
}

// ComposeSubscriptionModelPlan resolves and merges guardrails from the AITenant,
// MaasTenantConfig, subscription, matching subscription model entry, and
// MaaSModelRef.
//
// The caller must validate tenant ownership, subscription membership, and
// targetNamespace against AITenant.status.tenantNamespace.
// AITenant, MaasTenantConfig, and MaaSSubscription selections resolve in
// targetNamespace. MaaSModelRef selections resolve in their resource namespace.
// tenant.Namespace is used for provenance, not policy lookup.
//
// model must match modelNamespace/modelName. Requires exactly one matching model
// entry. Returns no partial plan on error.
func ComposeSubscriptionModelPlan(
	ctx context.Context,
	resolver *aiguardrail.AttachmentResolver,
	targetNamespace string,
	tenant *TenantSelection,
	tenantConfig *TenantConfigSelection,
	sub *SubscriptionSelection,
	model *ModelSelection,
	modelNamespace, modelName string,
) (SubscriptionModelKey, *ExecutionPlan, error) {
	if sub == nil {
		return SubscriptionModelKey{}, nil, fmt.Errorf("subscription is required: %w", ErrInvalidCompositionInput)
	}
	key := SubscriptionModelKey{
		SubscriptionNamespace: sub.Namespace,
		SubscriptionName:      sub.Name,
		ModelNamespace:        modelNamespace,
		ModelName:             modelName,
	}
	if resolver == nil {
		return key, nil, fmt.Errorf("compose %s: resolver is required: %w", key, ErrInvalidCompositionInput)
	}
	if sub.Namespace != targetNamespace {
		return key, nil, fmt.Errorf("compose %s: MaaSSubscription namespace %q does not match tenant target namespace %q: %w",
			key, sub.Namespace, targetNamespace, ErrInvalidCompositionInput)
	}
	if tenant == nil {
		return key, nil, fmt.Errorf("compose %s: AITenant is required: %w", key, ErrInvalidCompositionInput)
	}
	if tenantConfig == nil {
		return key, nil, fmt.Errorf("compose %s: MaasTenantConfig is required: %w", key, ErrInvalidCompositionInput)
	}
	if tenantConfig.Namespace != targetNamespace {
		return key, nil, fmt.Errorf("compose %s: MaasTenantConfig namespace %q does not match tenant target namespace %q: %w",
			key, tenantConfig.Namespace, targetNamespace, ErrInvalidCompositionInput)
	}
	if model == nil {
		return key, nil, fmt.Errorf("compose %s: MaaSModelRef is required: %w", key, ErrInvalidCompositionInput)
	}
	if model.Namespace != modelNamespace || model.Name != modelName {
		return key, nil, fmt.Errorf("compose %s: MaaSModelRef %s/%s does not match requested model %s/%s: %w",
			key, model.Namespace, model.Name, modelNamespace, modelName, ErrInvalidCompositionInput)
	}

	entry, err := findSubscriptionModelEntry(sub, modelNamespace, modelName)
	if err != nil {
		return key, nil, fmt.Errorf("compose %s: %w", key, err)
	}

	var selections []ResolvedSelection

	tenantSels, err := resolveAttachments(ctx, resolver, targetNamespace, key, tenant.Guardrails,
		func(i int) Source {
			return Source{
				Kind:           "AITenant",
				Namespace:      tenant.Namespace,
				Name:           tenant.Name,
				UID:            tenant.UID,
				AttachmentPath: fmt.Sprintf("AITenant/%s.spec.guardrails[%d]", tenant.Name, i),
			}
		})
	if err != nil {
		return key, nil, err
	}
	selections = append(selections, tenantSels...)

	tenantConfigSels, err := resolveAttachments(ctx, resolver, targetNamespace, key, tenantConfig.Guardrails,
		func(i int) Source {
			return Source{
				Kind:           "MaasTenantConfig",
				Namespace:      tenantConfig.Namespace,
				Name:           tenantConfig.Name,
				UID:            tenantConfig.UID,
				AttachmentPath: fmt.Sprintf("MaasTenantConfig/%s.spec.guardrails[%d]", tenantConfig.Name, i),
			}
		})
	if err != nil {
		return key, nil, err
	}
	selections = append(selections, tenantConfigSels...)

	subSels, err := resolveAttachments(ctx, resolver, targetNamespace, key, sub.Guardrails,
		func(i int) Source {
			return Source{
				Kind:           "MaaSSubscription",
				Namespace:      sub.Namespace,
				Name:           sub.Name,
				UID:            sub.UID,
				AttachmentPath: fmt.Sprintf("MaaSSubscription/%s.spec.guardrails[%d]", sub.Name, i),
			}
		})
	if err != nil {
		return key, nil, err
	}
	selections = append(selections, subSels...)

	modelSels, err := resolveAttachments(ctx, resolver, targetNamespace, key, entry.Guardrails,
		func(i int) Source {
			return Source{
				Kind:           "MaaSSubscription",
				Namespace:      sub.Namespace,
				Name:           sub.Name,
				UID:            sub.UID,
				AttachmentPath: fmt.Sprintf("MaaSSubscription/%s.spec.modelRefs[%s/%s].guardrails[%d]", sub.Name, modelNamespace, modelName, i),
			}
		})
	if err != nil {
		return key, nil, err
	}
	selections = append(selections, modelSels...)

	modelResourceSels, err := resolveAttachments(ctx, resolver, model.Namespace, key, model.Guardrails,
		func(i int) Source {
			return Source{
				Kind:           "MaaSModelRef",
				Namespace:      model.Namespace,
				Name:           model.Name,
				UID:            model.UID,
				AttachmentPath: fmt.Sprintf("MaaSModelRef/%s.spec.guardrails[%d]", model.Name, i),
			}
		})
	if err != nil {
		return key, nil, err
	}
	selections = append(selections, modelResourceSels...)

	plan, err := Merge(selections)
	if err != nil {
		return key, nil, fmt.Errorf("compose %s: %w", key, err)
	}
	return key, plan, nil
}

// resolveAttachments resolves each attachment in policyNamespace and tags it
// with the source built by sourceFn. It returns on the first resolution failure.
func resolveAttachments(
	ctx context.Context,
	resolver *aiguardrail.AttachmentResolver,
	policyNamespace string,
	key SubscriptionModelKey,
	attachments []aiguardrail.Attachment,
	sourceFn func(i int) Source,
) ([]ResolvedSelection, error) {
	selections := make([]ResolvedSelection, 0, len(attachments))
	for i := range attachments {
		source := sourceFn(i)
		resolved, err := resolver.Resolve(ctx, policyNamespace, attachments[i])
		if err != nil {
			return nil, fmt.Errorf("compose %s: resolve %s: %w", key, source.AttachmentPath, err)
		}
		selections = append(selections, ResolvedSelection{Policy: *resolved, Source: source})
	}
	return selections, nil
}

// findSubscriptionModelEntry returns the single subscription modelRefs entry
// matching namespace/name.
// A missing or ambiguous entry is an error rather than selecting another model.
func findSubscriptionModelEntry(sub *SubscriptionSelection, namespace, name string) (*ModelEntrySelection, error) {
	var match *ModelEntrySelection
	for i := range sub.ModelRefs {
		ref := &sub.ModelRefs[i]
		if ref.Namespace == namespace && ref.Name == name {
			if match != nil {
				return nil, fmt.Errorf("model %s/%s: %w", namespace, name, ErrModelEntryAmbiguous)
			}
			match = ref
		}
	}
	if match == nil {
		return nil, fmt.Errorf("model %s/%s: %w", namespace, name, ErrModelEntryNotFound)
	}
	return match, nil
}
