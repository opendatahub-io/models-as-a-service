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

// MaaSAuthPolicySpec defines the desired state of MaaSAuthPolicy
type MaaSAuthPolicySpec struct {
	// ModelRefs is a list of model names that this policy grants access to
	// +kubebuilder:validation:MinItems=1
	ModelRefs []string `json:"modelRefs"`

	// Subjects defines who has access (OR logic - any match grants access)
	Subjects SubjectSpec `json:"subjects"`

	// MeteringMetadata contains billing and tracking information
	// +optional
	MeteringMetadata *MeteringMetadata `json:"meteringMetadata,omitempty"`
}

// SubjectSpec defines the subjects that have access
type SubjectSpec struct {
	// Groups is a list of Kubernetes group names
	// +optional
	Groups []GroupReference `json:"groups,omitempty"`

	// Users is a list of Kubernetes user names
	// +optional
	Users []string `json:"users,omitempty"`
}

// GroupReference references a Kubernetes group
type GroupReference struct {
	// Name is the name of the group
	Name string `json:"name"`
}

// MeteringMetadata contains billing and tracking information
type MeteringMetadata struct {
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

// AuthPolicyRefStatus reports the status of one underlying Kuadrant AuthPolicy created by this MaaSAuthPolicy.
type AuthPolicyRefStatus struct {
	// Name is the name of the AuthPolicy resource.
	Name string `json:"name"`
	// Namespace is the namespace of the AuthPolicy resource.
	Namespace string `json:"namespace"`
	// Model is the model name this AuthPolicy targets.
	Model string `json:"model"`
	// Accepted reports whether the AuthPolicy has been accepted (e.g. status.conditions type=Accepted).
	// +optional
	Accepted string `json:"accepted,omitempty"`
	// Enforced reports whether the AuthPolicy is enforced (e.g. status.conditions type=Enforced).
	// +optional
	Enforced string `json:"enforced,omitempty"`
}

// MaaSAuthPolicyStatus defines the observed state of MaaSAuthPolicy
type MaaSAuthPolicyStatus struct {
	// Phase represents the current phase of the policy
	// +kubebuilder:validation:Enum=Pending;Active;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the policy's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AuthPolicies lists the underlying Kuadrant AuthPolicies and their Accepted/Enforced state.
	// +optional
	AuthPolicies []AuthPolicyRefStatus `json:"authPolicies,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
//+kubebuilder:printcolumn:name="AuthPolicies",type="string",JSONPath=".status.authPolicies[*].name",priority=1

// MaaSAuthPolicy is the Schema for the maasauthpolicies API
type MaaSAuthPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSAuthPolicySpec   `json:"spec,omitempty"`
	Status MaaSAuthPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MaaSAuthPolicyList contains a list of MaaSAuthPolicy
type MaaSAuthPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSAuthPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaaSAuthPolicy{}, &MaaSAuthPolicyList{})
}
