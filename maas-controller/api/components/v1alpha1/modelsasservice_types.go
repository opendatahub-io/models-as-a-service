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

const (
	ModelsAsServiceComponentName = "modelsasservice"
	ModelsAsServiceKind          = "ModelsAsService"
	ModelsAsServiceInstanceName  = "default-" + ModelsAsServiceComponentName
)

// ManagementState controls whether the MaaS module contract should be actively
// managed by the module operator.
type ManagementState string

const (
	ManagementStateManaged ManagementState = "Managed"
	ManagementStateRemoved ManagementState = "Removed"
)

// ModelsAsServicePhase is the top-level lifecycle summary exposed to the
// platform module handler contract.
type ModelsAsServicePhase string

const (
	ModelsAsServicePhaseReady    ModelsAsServicePhase = "Ready"
	ModelsAsServicePhaseNotReady ModelsAsServicePhase = "Not Ready"
)

// ComponentRelease reports a single installed component or operator release.
type ComponentRelease struct {
	// Name identifies the component or operator.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the installed version, when known.
	// +kubebuilder:validation:Optional
	Version string `json:"version,omitempty"`

	// RepoURL points at the source repository for the reported release, when known.
	// +kubebuilder:validation:Optional
	RepoURL string `json:"repoUrl,omitempty"`
}

// ModelsAsServiceSpec defines the desired state of the MaaS module contract.
// It intentionally stays minimal and does not mirror Tenant configuration.
type ModelsAsServiceSpec struct {
	// ManagementState controls whether the module operator should reconcile or remove
	// the MaaS platform resources represented by this contract.
	// +kubebuilder:default=Managed
	// +kubebuilder:validation:Enum=Managed;Removed
	ManagementState ManagementState `json:"managementState,omitempty"`
}

// ModelsAsServiceStatus defines the observed state of the MaaS module contract.
type ModelsAsServiceStatus struct {
	// Phase is a high-level lifecycle summary expected by the module handler.
	// Expected values are "Ready" and "Not Ready".
	// +kubebuilder:validation:Optional
	Phase ModelsAsServicePhase `json:"phase,omitempty"`

	// ObservedGeneration is the last metadata.generation processed by maas-controller.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available module-level observations.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Releases lists the installed operator / component versions reported by MaaS.
	// +optional
	// +listType=map
	// +listMapKey=name
	Releases []ComponentRelease `json:"releases,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-modelsasservice'",message="ModelsAsService name must be default-modelsasservice"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelsAsService is the canonical platform-facing contract for the MaaS module.
type ModelsAsService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelsAsServiceSpec   `json:"spec,omitempty"`
	Status ModelsAsServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelsAsServiceList contains a list of ModelsAsService.
type ModelsAsServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelsAsService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelsAsService{}, &ModelsAsServiceList{})
}
