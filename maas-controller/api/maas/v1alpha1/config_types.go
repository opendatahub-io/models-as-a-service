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
	// ConfigKind is the API kind for the cluster-scoped MaaS platform anchor.
	ConfigKind = "Config"
	// ConfigInstanceName is the singleton resource name enforced by the API.
	ConfigInstanceName = "default"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=maasconfig
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="Config name must be default"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Config is a cluster-scoped anchor for MaaS platform resources. Namespaced and
// cluster-scoped operands created by maas-controller reference this object as their controller
// owner so Kubernetes garbage collection can tear down the full graph when the Config
// is deleted (subject to finalizers on dependents).
type Config struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConfigSpec   `json:"spec,omitempty"`
	Status ConfigStatus `json:"status,omitempty"`
}

// ConfigSpec defines the desired state of Config.
// Reserved for future cluster-wide configuration; v1alpha1 uses an empty spec.
type ConfigSpec struct{}

// ConfigStatus defines the observed state of Config.
type ConfigStatus struct{}

// +kubebuilder:object:root=true

// ConfigList contains a list of Config.
type ConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Config `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Config{}, &ConfigList{})
}
