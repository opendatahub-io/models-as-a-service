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
	// ModelRefs is a list of models (by name and namespace) that this policy grants access to
	// +kubebuilder:validation:MinItems=1
	ModelRefs []ModelRef `json:"modelRefs"`

	// Subjects defines who has access (OR logic - any match grants access)
	// +kubebuilder:validation:XValidation:rule="size(self.groups) > 0 || size(self.users) > 0",message="at least one group or user must be specified in subjects"
	Subjects SubjectSpec `json:"subjects"`

	// MeteringMetadata contains billing and tracking information
	// +optional
	MeteringMetadata *MeteringMetadata `json:"meteringMetadata,omitempty"`
}

// ModelRef references a MaaSModelRef by name and namespace.
type ModelRef struct {
	// Name is the name of the MaaSModelRef
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Namespace is the namespace where the MaaSModelRef lives
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
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

// AuthPolicyRefStatus reports the status of a generated Kuadrant AuthPolicy.
// Embeds ResourceRefStatus for common fields (Ready, Reason, Message).
type AuthPolicyRefStatus struct {
	ResourceRefStatus `json:",inline"`
	// Model is the MaaSModelRef name this AuthPolicy targets.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Model string `json:"model"`
	// ModelNamespace is the namespace of the MaaSModelRef.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ModelNamespace string `json:"modelNamespace"`
}

// MaaSAuthPolicyStatus defines the observed state of MaaSAuthPolicy
type MaaSAuthPolicyStatus struct {
	// Phase represents the current phase of the policy
	Phase Phase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the policy's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AuthPolicies lists the underlying Kuadrant AuthPolicies and their status.
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
