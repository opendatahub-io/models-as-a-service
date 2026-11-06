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

// Package guardrail composes and merges MaaS guardrail selections.
package guardrail

import (
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	"k8s.io/apimachinery/pkg/types"
)

// These inputs project guardrail fields from maas-controller/api/maas/v1alpha1
// without making this shared module depend on the controller module.
// When the corresponding CRD guardrail fields or supported scopes change,
// update these inputs and the consuming controller/API conversion code together.

// ResourceRef records the resource metadata used for provenance.
type ResourceRef struct {
	Namespace string
	Name      string
	UID       types.UID
}

// TenantSelection supplies AITenant baseline guardrails.
type TenantSelection struct {
	ResourceRef
	Guardrails []aiguardrail.Attachment
}

// TenantConfigSelection supplies MaasTenantConfig baseline guardrails.
type TenantConfigSelection struct {
	ResourceRef
	Guardrails []aiguardrail.Attachment
}

// SubscriptionSelection supplies MaaSSubscription global and model guardrails.
type SubscriptionSelection struct {
	ResourceRef
	Guardrails []aiguardrail.Attachment
	ModelRefs  []ModelEntrySelection
}

// ModelEntrySelection supplies guardrails from a subscription modelRefs entry.
type ModelEntrySelection struct {
	Namespace  string
	Name       string
	Guardrails []aiguardrail.Attachment
}

// ModelSelection supplies MaaSModelRef guardrails for the selected model.
type ModelSelection struct {
	ResourceRef
	Guardrails []aiguardrail.Attachment
}
