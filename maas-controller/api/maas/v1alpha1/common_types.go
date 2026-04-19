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

// Phase represents the lifecycle phase of a MaaS resource.
// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
type Phase string

// Phase constants for MaaS resources (MaaSSubscription, MaaSAuthPolicy, MaaSModelRef)
const (
	PhasePending  Phase = "Pending"
	PhaseActive   Phase = "Active"
	PhaseDegraded Phase = "Degraded"
	PhaseFailed   Phase = "Failed"
)

// ConditionReason represents a machine-readable reason for a status condition.
// +kubebuilder:validation:Enum=Reconciled;ReconcileFailed;PartialFailure;Valid;NotFound;GetFailed;Accepted;AcceptedEnforced;NotAccepted;Enforced;NotEnforced;BackendNotReady;ConditionsNotFound;Unknown
type ConditionReason string

// Reason constants for status conditions and per-item statuses.
// These follow Kubernetes conventions: CamelCase, past tense for completed actions.
const (
	// ReasonReconciled indicates successful reconciliation.
	ReasonReconciled ConditionReason = "Reconciled"

	// ReasonReconcileFailed indicates reconciliation failed.
	ReasonReconcileFailed ConditionReason = "ReconcileFailed"

	// ReasonPartialFailure indicates some items succeeded, others failed.
	ReasonPartialFailure ConditionReason = "PartialFailure"

	// ReasonValid indicates a referenced resource exists and is valid.
	ReasonValid ConditionReason = "Valid"

	// ReasonNotFound indicates a referenced resource was not found.
	ReasonNotFound ConditionReason = "NotFound"

	// ReasonGetFailed indicates a failure when fetching a resource.
	ReasonGetFailed ConditionReason = "GetFailed"

	// ReasonAccepted indicates the resource was accepted by the target system (e.g., Kuadrant).
	ReasonAccepted ConditionReason = "Accepted"

	// ReasonAcceptedEnforced indicates the policy is both accepted and enforced.
	ReasonAcceptedEnforced ConditionReason = "AcceptedEnforced"

	// ReasonNotAccepted indicates the resource was not accepted by the target system.
	ReasonNotAccepted ConditionReason = "NotAccepted"

	// ReasonEnforced indicates the policy is actively enforced.
	ReasonEnforced ConditionReason = "Enforced"

	// ReasonNotEnforced indicates the policy is not yet enforced.
	ReasonNotEnforced ConditionReason = "NotEnforced"

	// ReasonBackendNotReady indicates the backend service is not ready.
	ReasonBackendNotReady ConditionReason = "BackendNotReady"

	// ReasonConditionsNotFound indicates status conditions are not available.
	ReasonConditionsNotFound ConditionReason = "ConditionsNotFound"

	// ReasonUnknown indicates an unknown or unhandled state.
	ReasonUnknown ConditionReason = "Unknown"
)

// ResourceRefStatus is the common status for any referenced Kubernetes resource.
// Embedded by specific status types for type safety (follows metav1.Condition pattern).
type ResourceRefStatus struct {
	// Name of the referenced resource
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespace of the referenced resource
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// Ready indicates whether the resource is valid and healthy
	Ready bool `json:"ready"`
	// Reason is a machine-readable reason code
	// +optional
	Reason ConditionReason `json:"reason,omitempty"`
	// Message is a human-readable description of the status
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}
