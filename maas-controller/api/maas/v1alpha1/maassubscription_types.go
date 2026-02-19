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
)

// MaaSSubscriptionSpec defines the desired state of MaaSSubscription
type MaaSSubscriptionSpec struct {
	// Owner defines who owns this subscription
	Owner OwnerSpec `json:"owner"`

	// ModelRefs defines which models are included with per-model token rate limits
	// +kubebuilder:validation:MinItems=1
	ModelRefs []ModelSubscriptionRef `json:"modelRefs"`

	// BillingMetadata contains billing information
	// +optional
	BillingMetadata *BillingMetadata `json:"billingMetadata,omitempty"`

	// Priority determines subscription priority when user has multiple subscriptions
	// Higher numbers have higher priority. Defaults to 0.
	// +optional
	// +kubebuilder:default=0
	Priority int32 `json:"priority,omitempty"`
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
type ModelSubscriptionRef struct {
	// Name is the name of the model
	Name string `json:"name"`

	// TokenRateLimits defines token-based rate limits for this model
	// +optional
	TokenRateLimits []TokenRateLimit `json:"tokenRateLimits,omitempty"`

	// TokenRateLimitRef references an existing TokenRateLimit resource
	// +optional
	TokenRateLimitRef *string `json:"tokenRateLimitRef,omitempty"`

	// BillingRate defines the cost per token
	// +optional
	BillingRate *BillingRate `json:"billingRate,omitempty"`
}

// TokenRateLimit defines a token rate limit
type TokenRateLimit struct {
	// Limit is the maximum number of tokens allowed
	Limit int64 `json:"limit"`

	// Window is the time window (e.g., "1m", "1h", "24h")
	// +kubebuilder:validation:Pattern=`^(\d+)(s|m|h|d)$`
	Window string `json:"window"`
}

// BillingRate defines billing information
type BillingRate struct {
	// PerToken is the cost per token
	PerToken string `json:"perToken"`
}

// BillingMetadata contains billing information
type BillingMetadata struct {
	// OrganizationID is the organization identifier for billing
	// +optional
	OrganizationID string `json:"organizationId,omitempty"`

	// CostCenter is the cost center for billing attribution
	// +optional
	CostCenter string `json:"costCenter,omitempty"`

	// Labels are additional labels for tracking
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// MaaSSubscriptionStatus defines the observed state of MaaSSubscription
type MaaSSubscriptionStatus struct {
	// Phase represents the current phase of the subscription
	// +kubebuilder:validation:Enum=Pending;Active;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the subscription's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

	Spec   MaaSSubscriptionSpec   `json:"spec,omitempty"`
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
	SchemeBuilder.Register(&MaaSSubscription{}, &MaaSSubscriptionList{})
}
