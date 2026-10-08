/*
Copyright 2025.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// MaaSSubscriptionSpec defines the desired state of MaaSSubscription
type MaaSSubscriptionSpec struct {
	// Owner defines who owns this subscription
	Owner OwnerSpec `json:"owner"`

	// ModelRefs defines which models are included with per-model token rate limits
	// +kubebuilder:validation:MinItems=1
	ModelRefs []ModelSubscriptionRef `json:"modelRefs"`

	// TokenMetadata contains metadata for token attribution and metering
	// +optional
	TokenMetadata *TokenMetadata `json:"tokenMetadata,omitempty"`

	// Priority determines subscription priority when user has multiple subscriptions
	// Higher numbers have higher priority. Defaults to 0.
	// +optional
	// +kubebuilder:default=0
	Priority int32 `json:"priority,omitempty"`

	// InferencePriority is the scheduling priority applied to inference requests made under
	// this subscription, published as InferenceObjective.spec.priority.
	// Higher values are scheduled ahead of lower ones; negative values are scheduled below
	// the scheduler default. Distinct from Priority, which selects among a user's
	// subscriptions and has no effect on inference scheduling.
	// When unset, no InferenceObjective is created and the scheduler applies priority 0.
	// An explicit 0 creates an InferenceObjective with priority 0.
	// +optional
	// +kubebuilder:validation:Minimum=-2147483648
	// +kubebuilder:validation:Maximum=2147483647
	InferencePriority *int32 `json:"inferencePriority,omitempty"`

	// Guardrails attaches reusable AIGuardrail policies for every model accessed
	// through this subscription. References resolve in the AITenant target
	// namespace; a namespace must not be specified on the reference. Selections
	// are additive with the tenant baseline and cannot remove checks contributed
	// by another scope.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Guardrails []GuardrailAttachment `json:"guardrails,omitempty"`
}

// OwnerSpec defines the owner of the subscription
type OwnerSpec struct {
	// Groups is a list of Kubernetes group names that own this subscription
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// Users is a list of Kubernetes user names that own this subscription
	// +optional
	Users []string `json:"users,omitempty"`
}

// ModelSubscriptionRef defines a model reference with rate limits
// +kubebuilder:validation:XValidation:rule="has(self.tokenRateLimits) || (has(self.unlimited) && self.unlimited)",message="tokenRateLimits is required unless unlimited is true"
// +kubebuilder:validation:XValidation:rule="!(has(self.unlimited) && self.unlimited && has(self.tokenRateLimits))",message="tokenRateLimits must not be set when unlimited is true"
type ModelSubscriptionRef struct {
	// Name is the name of the MaaSModelRef
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Namespace is the namespace where the MaaSModelRef lives
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// TokenRateLimits defines token-based rate limits for this model.
	// Required unless Unlimited is true.
	// +optional
	// +kubebuilder:validation:MinItems=1
	TokenRateLimits []TokenRateLimit `json:"tokenRateLimits,omitempty"`

	// Unlimited grants access to this model without a token budget.
	// Token usage is still metered. Mutually exclusive with TokenRateLimits.
	// +optional
	Unlimited bool `json:"unlimited,omitempty"`

	// BillingRate defines the cost per token
	// +optional
	BillingRate *BillingRate `json:"billingRate,omitempty"`

	// Guardrails attaches reusable AIGuardrail policies for this specific model
	// within the subscription. References resolve in the AITenant target
	// namespace; a namespace must not be specified on the reference. Selections
	// are additive with the subscription-wide guardrails and every other scope
	// and cannot remove checks contributed elsewhere.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Guardrails []GuardrailAttachment `json:"guardrails,omitempty"`
}

// TokenRateLimit defines a token rate limit
type TokenRateLimit struct {
	// Limit is the maximum number of tokens allowed within the window.
	// Must be between 1 and 1,000,000,000 (1 billion).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000000
	Limit int64 `json:"limit"`

	// Window is the time window for rate limiting (e.g., "1m", "1h", "24h").
	// Allowed units: s (seconds), m (minutes), h (hours). Days (d) are not
	// supported; use hours instead (e.g., "24h" for one day).
	// The numeric part must be between 1 and 9999.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=5
	// +kubebuilder:validation:Pattern=`^[1-9]\d{0,3}(s|m|h)$`
	Window string `json:"window"`
}

// BillingRate defines billing information
type BillingRate struct {
	// PerToken is the cost per token
	PerToken string `json:"perToken"`
}

// TokenMetadata contains metadata for token usage attribution and metering
type TokenMetadata struct {
	// OrganizationID is the organization identifier for metering and billing
	// +optional
	OrganizationID string `json:"organizationId,omitempty"`

	// CostCenter is the cost center for usage attribution
	// +optional
	CostCenter string `json:"costCenter,omitempty"`

	// Labels are additional labels for tracking and metrics
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// ModelRefStatus reports the status of a referenced MaaSModelRef.
type ModelRefStatus struct {
	ResourceRefStatus `json:",inline"`
}

// TokenRateLimitStatus reports the status of a generated TokenRateLimitPolicy.
type TokenRateLimitStatus struct {
	ResourceRefStatus `json:",inline"`
	// Model is the MaaSModelRef name this TokenRateLimitPolicy targets
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Model string `json:"model"`
}

// ConditionInferenceObjectivesReady reports whether the InferenceObjectives for
// spec.inferencePriority are reconciled. It is only set when spec.inferencePriority is set.
const ConditionInferenceObjectivesReady = "InferenceObjectivesReady"

// FlowControlReason is a machine-readable reason for a model's request-priority status.
// It is intentionally not an enum in the CRD so reasons can be added without a CRD update.
// +kubebuilder:validation:MaxLength=64
type FlowControlReason string

const (
	// FlowControlReasonObjectiveReconciled indicates the model's InferenceObjective matches
	// spec.inferencePriority. Ready is true.
	FlowControlReasonObjectiveReconciled FlowControlReason = "ObjectiveReconciled"
	// FlowControlReasonPriorityUnset indicates spec.inferencePriority is unset, so no
	// InferenceObjective is required and the scheduler applies priority 0. Ready is true.
	FlowControlReasonPriorityUnset FlowControlReason = "PriorityUnset"
	// FlowControlReasonNotApplicable indicates the model is not served through an inference
	// scheduler (for example, an ExternalModel), so request priority does not apply. Ready is true.
	FlowControlReasonNotApplicable FlowControlReason = "NotApplicable"
	// FlowControlReasonUnmanaged indicates the InferenceObjective opted out of management
	// (opendatahub.io/managed: "false"); its priority is owned by the user. Ready is true.
	FlowControlReasonUnmanaged FlowControlReason = "Unmanaged"
	// FlowControlReasonPoolPending indicates the model's InferencePool or HTTPRoute has not
	// been observed yet.
	FlowControlReasonPoolPending FlowControlReason = "PoolPending"
	// FlowControlReasonObjectivePending indicates the InferenceObjective is not reconciled yet.
	FlowControlReasonObjectivePending FlowControlReason = "ObjectivePending"
	// FlowControlReasonUnsupported indicates request priority cannot be applied to the model, for
	// example because its traffic is split across multiple InferencePools.
	FlowControlReasonUnsupported FlowControlReason = "Unsupported"
	// FlowControlReasonNotOnTenantGateway indicates the model's HTTPRoute is not attached to the
	// subscription's tenant Gateway.
	FlowControlReasonNotOnTenantGateway FlowControlReason = "NotOnTenantGateway"
	// FlowControlReasonObjectiveConflict indicates another object holds the InferenceObjective name.
	FlowControlReasonObjectiveConflict FlowControlReason = "ObjectiveConflict"
	// FlowControlReasonAPIUnavailable indicates the InferenceObjective API is not installed.
	FlowControlReasonAPIUnavailable FlowControlReason = "InferenceObjectiveAPIUnavailable"
	// FlowControlReasonReconcileFailed indicates reconciling the model's request priority failed.
	FlowControlReasonReconcileFailed FlowControlReason = "ReconcileFailed"
)

// ModelFlowControlStatus maps a referenced model to its InferencePool and InferenceObjective.
type ModelFlowControlStatus struct {
	// Name of the MaaSModelRef
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace of the MaaSModelRef. The InferencePool and InferenceObjective live in this
	// namespace, alongside the model's LLMInferenceService.
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// InferencePool is the pool observed in the model's LLMInferenceService status
	// +optional
	InferencePool *gwapiv1.LocalObjectReference `json:"inferencePool,omitempty"`
	// ObjectiveName is the InferenceObjective name for this subscription and pool, in the pool's
	// namespace. It is set whenever the pool is known, including when spec.inferencePriority is unset
	// and no InferenceObjective is created.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	ObjectiveName string `json:"objectiveName,omitempty"`
	// Ready is true when no request-priority reconciliation is left for this model: its
	// InferenceObjective matches spec.inferencePriority, spec.inferencePriority is unset, request
	// priority does not apply, or the InferenceObjective opted out of management. It does not
	// report backend readiness; see ModelRefStatuses.
	Ready bool `json:"ready"`
	// Reason is a machine-readable reason for Ready
	// +optional
	Reason FlowControlReason `json:"reason,omitempty"`
	// Message is a human-readable description of the reason
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}

// MaaSSubscriptionStatus defines the observed state of MaaSSubscription
type MaaSSubscriptionStatus struct {
	// Phase represents the current phase of the subscription
	Phase Phase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the subscription's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ModelRefStatuses reports the status of each referenced MaaSModelRef
	// +optional
	ModelRefStatuses []ModelRefStatus `json:"modelRefStatuses,omitempty"`

	// TokenRateLimitStatuses reports the status of each generated TokenRateLimitPolicy
	// +optional
	TokenRateLimitStatuses []TokenRateLimitStatus `json:"tokenRateLimitStatuses,omitempty"`

	// FlowControlStatuses reports, for each referenced model, the InferencePool and
	// InferenceObjective used for request priority, and their reconciliation state
	// +optional
	FlowControlStatuses []ModelFlowControlStatus `json:"flowControlStatuses,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Priority",type="integer",JSONPath=".spec.priority"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MaaSSubscription is the Schema for the maassubscriptions API
type MaaSSubscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSSubscriptionSpec   `json:"spec"`
	Status MaaSSubscriptionStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MaaSSubscriptionList contains a list of MaaSSubscription
type MaaSSubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSSubscription `json:"items"`
}

func init() {
	register(&MaaSSubscription{}, &MaaSSubscriptionList{})
}
