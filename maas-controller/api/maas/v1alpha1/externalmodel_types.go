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

// ExternalModelSpec defines the desired state of ExternalModel
type ExternalModelSpec struct {
	// Provider identifies the API format and auth type for the external model.
	// e.g. "openai", "anthropic".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Provider string `json:"provider"`

	// Endpoint is the FQDN of the external provider (no scheme or path).
	// e.g. "api.openai.com".
	// This field is metadata for downstream consumers (e.g. BBR provider-resolver plugin)
	// and is not used by the controller for endpoint derivation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]*[a-zA-Z0-9])?)*$`
	Endpoint string `json:"endpoint"`

	// CredentialRef references a Kubernetes Secret containing the provider API key.
	// The Secret must contain a data key "api-key" with the credential value.
	// +kubebuilder:validation:Required
	CredentialRef CredentialReference `json:"credentialRef"`

	// BackendModelName is the actual model identifier used by the external provider.
	// When specified, client requests using this ExternalModel's name (metadata.name)
	// will be translated to use this backend model name in API calls.
	// If not specified, the metadata.name is used as the backend model name.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	BackendModelName string `json:"backendModelName,omitempty"`

	// ModelAliases defines additional virtual names that clients can use to reference this model.
	// These aliases, along with the metadata.name, will all resolve to the same backend model.
	// Each alias must be unique across all ExternalModel and MaaSModelRef resources in the cluster.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:UniqueItems=true
	ModelAliases []string `json:"modelAliases,omitempty"`
}

// ExternalModelStatus defines the observed state of ExternalModel
type ExternalModelStatus struct {
	// Phase represents the current phase of the external model
	// +kubebuilder:validation:Enum=Pending;Ready;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the external model's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// VirtualNames lists all virtual names (metadata.name + aliases) that resolve to this model.
	// This field is populated by the controller for observability.
	// +optional
	VirtualNames []string `json:"virtualNames,omitempty"`

	// ResolvedBackendModelName shows the resolved backend model name being used.
	// This field is populated by the controller for observability.
	// +optional
	ResolvedBackendModelName string `json:"resolvedBackendModelName,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
//+kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.endpoint"
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ExternalModel is the Schema for the externalmodels API.
// It defines an external LLM provider (e.g., OpenAI, Anthropic) that can be
// referenced by MaaSModelRef resources.
//
// Model Name Virtualization:
// ExternalModel supports model name virtualization where the Kubernetes resource name
// (metadata.name) serves as a virtual model name that clients use in API requests.
// The actual provider-specific model identifier is specified in spec.backendModelName.
// Additional virtual names can be defined via spec.modelAliases.
//
// Example:
//   metadata:
//     name: claude                    # Virtual name clients use
//   spec:
//     provider: anthropic
//     backendModelName: claude-3-5-sonnet-20241022  # Real provider model
//     modelAliases: ["claude-3", "claude-sonnet"]   # Additional virtual names
//
// With this configuration, client requests using "claude", "claude-3", or "claude-sonnet"
// will be automatically translated to use "claude-3-5-sonnet-20241022" when sent to the Anthropic API.
type ExternalModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ExternalModelSpec   `json:"spec,omitempty"`
	Status ExternalModelStatus `json:"status,omitempty"`
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
