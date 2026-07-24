/*
Copyright 2026.

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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// MaasTenantConfigKind is the API kind for MaaS-specific tenant configuration.
	MaasTenantConfigKind = "MaasTenantConfig"
	// MaasTenantConfigInstanceName is the singleton resource name enforced by the API.
	MaasTenantConfigInstanceName = "default-tenant"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-tenant'",message="MaasTenantConfig name must be default-tenant"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="InfraNamespace",type=string,JSONPath=`.status.infraNamespace`,priority=1

// MaasTenantConfig is the namespace-scoped singleton for MaaS-specific tenant settings.
// Platform context such as Gateway and OIDC belongs to AITenant; this object only
// owns MaaS runtime configuration such as API key policy and telemetry settings.
type MaasTenantConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaasTenantConfigSpec   `json:"spec,omitempty"`
	Status MaasTenantConfigStatus `json:"status,omitempty"`
}

// MaasTenantConfigSpec defines MaaS-owned tenant configuration.
type MaasTenantConfigSpec struct {
	// APIKeys contains configuration for API key management.
	// +kubebuilder:validation:Optional
	APIKeys *TenantAPIKeysConfig `json:"apiKeys,omitempty"`

	// Telemetry contains configuration for telemetry and metrics collection.
	// +kubebuilder:validation:Optional
	Telemetry *TenantTelemetryConfig `json:"telemetry,omitempty"`
}

// MaasTenantConfigStatus defines the observed state of MaasTenantConfig.
type MaasTenantConfigStatus struct {
	// Phase is a high-level lifecycle phase for the platform reconcile.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// InfraNamespace is the infrastructure namespace where maas-api and the
	// maas-db-config secret are deployed for this tenant.
	// When credential rotation is needed, update the maas-db-config secret
	// in this namespace.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	InfraNamespace string `json:"infraNamespace,omitempty"`

	// Conditions represent the latest available observations.
	// Types mirror ODH modelsasservice / maas-controller status for DSC aggregation: Ready,
	// DependenciesAvailable, MaaSPrerequisitesAvailable, DeploymentsAvailable, Degraded.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MaasTenantConfigList contains a list of MaasTenantConfig.
type MaasTenantConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaasTenantConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaasTenantConfig{}, &MaasTenantConfigList{})
}
