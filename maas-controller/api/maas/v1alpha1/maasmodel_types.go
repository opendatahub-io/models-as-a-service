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

// MaaSModelSpec defines the desired state of MaaSModel
type MaaSModelSpec struct {
	// ModelRef references the actual model endpoint
	ModelRef ModelReference `json:"modelRef"`
}

// ModelReference references a model endpoint
type ModelReference struct {
	// Kind determines which fields are available
	// +kubebuilder:validation:Enum=llmisvc;ExternalModel
	Kind string `json:"kind"`

	// Name is the name of the model resource
	Name string `json:"name"`

	// Namespace is the namespace of the model resource
	// If not specified, defaults to the same namespace as MaaSModel
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// MaaSModelStatus defines the observed state of MaaSModel
type MaaSModelStatus struct {
	// Phase represents the current phase of the model
	// +kubebuilder:validation:Enum=Pending;Ready;Unhealthy;Failed
	Phase string `json:"phase,omitempty"`

	// Endpoint is the endpoint URL for the model
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// HTTPRouteName is the name of the HTTPRoute associated with this model
	// +optional
	HTTPRouteName string `json:"httpRouteName,omitempty"`

	// HTTPRouteNamespace is the namespace of the HTTPRoute associated with this model
	// +optional
	HTTPRouteNamespace string `json:"httpRouteNamespace,omitempty"`

	// HTTPRouteGatewayName is the name of the Gateway that the HTTPRoute references
	// +optional
	HTTPRouteGatewayName string `json:"httpRouteGatewayName,omitempty"`

	// HTTPRouteGatewayNamespace is the namespace of the Gateway that the HTTPRoute references
	// +optional
	HTTPRouteGatewayNamespace string `json:"httpRouteGatewayNamespace,omitempty"`

	// HTTPRouteHostnames are the hostnames configured on the HTTPRoute
	// +optional
	HTTPRouteHostnames []string `json:"httpRouteHostnames,omitempty"`

	// Conditions represent the latest available observations of the model's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
//+kubebuilder:printcolumn:name="HTTPRoute",type="string",JSONPath=".status.httpRouteName"
//+kubebuilder:printcolumn:name="Gateway",type="string",JSONPath=".status.httpRouteGatewayName"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MaaSModel is the Schema for the maasmodels API
type MaaSModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSModelSpec   `json:"spec,omitempty"`
	Status MaaSModelStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MaaSModelList contains a list of MaaSModel
type MaaSModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaaSModel{}, &MaaSModelList{})
}
