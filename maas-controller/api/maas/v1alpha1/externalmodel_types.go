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

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
//+kubebuilder:printcolumn:name="TargetModel",type="string",JSONPath=".spec.targetModel"
//+kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.endpoint"
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ExternalModel is the Schema for the externalmodels API.
// It defines an external LLM provider (e.g., OpenAI, Anthropic) that can be
// referenced by MaaSModelRef resources.
type ExternalModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExternalModelSpec   `json:"spec,omitempty"`
	Status ExternalModelStatus `json:"status,omitempty"`
}

// ExternalModelSpec defines the desired state of ExternalModel
type ExternalModelSpec struct {
	// Provider identifies the API format and auth type for the external model.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Provider string `json:"provider"`

	// TargetModel is the upstream model name at the external provider.
	// e.g. "gpt-4o", "claude-sonnet-4-5-20241022".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	TargetModel string `json:"targetModel"`

	// Endpoint is the FQDN of the external provider (no scheme or path).
	// e.g. "api.openai.com".
	// This field is metadata for downstream consumers (e.g. BBR provider-resolver plugin)
	// and is not used by the controller for endpoint derivation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?)*$`
	Endpoint string `json:"endpoint"`

	// CredentialRef references a Kubernetes Secret containing the provider API key.
	// The Secret must contain a data key "api-key" with the credential value.
	// +kubebuilder:validation:Required
	CredentialRef CredentialReference `json:"credentialRef"`
}

// CredentialReference references a Kubernetes Secret with provider API credentials.
// The Secret must be in the same namespace as the ExternalModel.
type CredentialReference struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ExternalModelStatus defines the observed state of ExternalModel
type ExternalModelStatus struct {
	// Phase represents the current phase of the external model
	// +kubebuilder:validation:Enum=Pending;Ready;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the external model's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true

// ExternalModelList contains a list of ExternalModel
type ExternalModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExternalModel{}, &ExternalModelList{})
}
