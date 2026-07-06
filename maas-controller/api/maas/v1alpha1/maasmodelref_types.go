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
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".status.endpoint"
//+kubebuilder:printcolumn:name="HTTPRoute",type="string",JSONPath=".status.httpRouteName"
//+kubebuilder:printcolumn:name="Gateway",type="string",JSONPath=".status.httpRouteGatewayName"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MaaSModelRef is the Schema for the maasmodelrefs API
type MaaSModelRef struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSModelSpec   `json:"spec"`
	Status MaaSModelStatus `json:"status,omitempty"`
}

// MaaSModelSpec defines the desired state of MaaSModelRef
type MaaSModelSpec struct {
	// ModelRef references the actual model endpoint
	ModelRef ModelReference `json:"modelRef"`
	// EndpointOverride, when set, overrides the endpoint URL that the controller
	// would otherwise discover from the backend (e.g. LLMInferenceService status
	// or Gateway/HTTPRoute).
	// +optional
	EndpointOverride string `json:"endpointOverride,omitempty"`
}

// ModelReference references a model endpoint in the same namespace.
// For kind=ExternalModel, the Name field references an ExternalModel CR in the same namespace.
type ModelReference struct {
	// Kind determines which backend handles this model reference.
	// LLMInferenceService: references a KServe LLMInferenceService.
	// ExternalModel: references an ExternalModel CR containing provider config.
	// +kubebuilder:validation:Enum=LLMInferenceService;ExternalModel
	Kind string `json:"kind"`

	// Name is the name of the model resource.
	// For LLMInferenceService, this is the InferenceService name.
	// For ExternalModel, this is the ExternalModel CR name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// MaaSModelStatus defines the observed state of MaaSModelRef.
//
// Phase semantics with governance:
//   - Pending: the model is not yet ready — either the backend is initializing
//     or no active MaaSSubscription + MaaSAuthPolicy pairing has been found.
//   - Ready: the model backend is healthy AND at least one governance pairing
//     (MaaSSubscription + MaaSAuthPolicy) is active. Authorized inference is possible.
//   - Unhealthy: the model has active governance but the backend (routes, gateways,
//     or inference service) has a runtime/health failure. GovernanceAttached remains
//     True while RuntimeReady is False.
//   - Failed: a non-recoverable reconciliation error occurred.
//   - Invalid: the resource spec is missing or structurally invalid.
//
// Condition types:
//   - Ready: overall readiness (True only when both governance and runtime are healthy).
//   - GovernanceAttached: whether the model is covered by at least one active
//     MaaSSubscription + MaaSAuthPolicy pairing. No admin CR names, namespaces,
//     or UIDs appear in any status field.
//   - RuntimeReady: whether the model backend is healthy and serving, independent
//     of governance state.
type MaaSModelStatus struct {
	// Phase represents the current phase of the model.
	// Pending = awaiting governance pairing or backend readiness.
	// Ready = governed and runtime-healthy.
	// Unhealthy = governed but runtime-failed.
	// Failed = reconciliation error.
	// Invalid = bad spec.
	// +kubebuilder:validation:Enum=Pending;Ready;Unhealthy;Failed;Invalid
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

	// Conditions represent the latest available observations of the model's state.
	// Condition types include:
	//   - Ready: overall readiness (governance + runtime).
	//   - GovernanceAttached: active MaaSSubscription + MaaSAuthPolicy pairing exists.
	//   - RuntimeReady: backend is healthy and serving.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true

// MaaSModelRefList contains a list of MaaSModelRef
type MaaSModelRefList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSModelRef `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaaSModelRef{}, &MaaSModelRefList{})
}
