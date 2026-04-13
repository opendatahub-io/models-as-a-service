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
	// MaaSTenantKind is the API kind for the cluster MaaS tenant / platform singleton.
	MaaSTenantKind = "MaaSTenant"
	// MaaSTenantInstanceName is the singleton resource name enforced by the API.
	MaaSTenantInstanceName = "default-maastenant"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-maastenant'",message="MaaSTenant name must be default-maastenant"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MaaSTenant is the cluster-scoped API for the MaaS platform tenant (replaces ModelsAsService for control-plane ownership).
type MaaSTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSTenantSpec   `json:"spec,omitempty"`
	Status MaaSTenantStatus `json:"status,omitempty"`
}

// MaaSTenantSpec defines the desired state of MaaSTenant (spec parity with ModelsAsService).
type MaaSTenantSpec struct {
	// GatewayRef specifies which Gateway (Gateway API) to use for exposing model endpoints.
	// If omitted, defaults to openshift-ingress/maas-default-gateway.
	// +kubebuilder:validation:Optional
	GatewayRef TenantGatewayRef `json:"gatewayRef,omitempty"`

	// APIKeys contains configuration for API key management.
	// +kubebuilder:validation:Optional
	APIKeys *TenantAPIKeysConfig `json:"apiKeys,omitempty"`

	// ExternalOIDC configures an external OIDC identity provider for the maas-api AuthPolicy.
	// +kubebuilder:validation:Optional
	ExternalOIDC *TenantExternalOIDCConfig `json:"externalOIDC,omitempty"`

	// Telemetry contains configuration for telemetry and metrics collection.
	// +kubebuilder:validation:Optional
	Telemetry *TenantTelemetryConfig `json:"telemetry,omitempty"`
}

// TenantExternalOIDCConfig defines the external OIDC provider settings.
type TenantExternalOIDCConfig struct {
	// IssuerURL is the OIDC issuer URL (e.g. https://keycloak.example.com/realms/maas).
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://\S+$`
	IssuerURL string `json:"issuerUrl"`

	// ClientID is the OAuth2 client ID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^\S+$`
	ClientID string `json:"clientId"`

	// TTL is the JWKS cache duration in seconds.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	TTL int `json:"ttl,omitempty"`
}

// TenantTelemetryConfig defines configuration for telemetry collection.
type TenantTelemetryConfig struct {
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	Enabled *bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:Optional
	Metrics *TenantMetricsConfig `json:"metrics,omitempty"`
}

// TenantMetricsConfig defines optional metric dimensions.
type TenantMetricsConfig struct {
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	CaptureOrganization *bool `json:"captureOrganization,omitempty"`

	// +kubebuilder:default=false
	// +kubebuilder:validation:Optional
	CaptureUser *bool `json:"captureUser,omitempty"`

	// +kubebuilder:default=false
	// +kubebuilder:validation:Optional
	CaptureGroup *bool `json:"captureGroup,omitempty"`

	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	CaptureModelUsage *bool `json:"captureModelUsage,omitempty"`
}

// TenantAPIKeysConfig defines configuration options for API key management.
type TenantAPIKeysConfig struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	MaxExpirationDays *int32 `json:"maxExpirationDays,omitempty"`
}

// TenantGatewayRef defines the reference to the global Gateway (Gateway API).
type TenantGatewayRef struct {
	// +kubebuilder:default="openshift-ingress"
	// +kubebuilder:validation:Pattern="^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$"
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`

	// +kubebuilder:default="maas-default-gateway"
	// +kubebuilder:validation:Pattern="^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$"
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name,omitempty"`
}

// MaaSTenantStatus defines the observed state of MaaSTenant.
type MaaSTenantStatus struct {
	// Phase is a high-level lifecycle phase for the platform reconcile.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// Types mirror ODH modelsasservice / internal controller status for DSC aggregation: Ready,
	// DependenciesAvailable, MaaSPrerequisitesAvailable, DeploymentsAvailable, Degraded.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MaaSTenantList contains a list of MaaSTenant.
type MaaSTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaaSTenant{}, &MaaSTenantList{})
}
