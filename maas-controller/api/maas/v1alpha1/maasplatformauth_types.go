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

// MaaSPlatformAuthSpec defines the desired state of MaaSPlatformAuth.
type MaaSPlatformAuthSpec struct {
	// ExternalOIDC configures external OIDC identity provider integration.
	// When set, the maas-api AuthPolicy is configured to accept JWT tokens from
	// the specified issuer alongside OpenShift TokenReview and API key authentication.
	// +optional
	ExternalOIDC *ExternalOIDCConfig `json:"externalOIDC,omitempty"`
}

// ExternalOIDCConfig defines the external OIDC provider settings.
type ExternalOIDCConfig struct {
	// IssuerURL is the OIDC issuer URL (e.g., https://keycloak.example.com/realms/maas).
	// Must serve a .well-known/openid-configuration endpoint.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://`
	IssuerURL string `json:"issuerUrl"`

	// ClientID is the OAuth2 client ID. Incoming OIDC tokens must have an
	// azp (authorized party) claim matching this value.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientId"`

	// TTL is the JWKS cache duration in seconds.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	TTL int `json:"ttl,omitempty"`
}

// MaaSPlatformAuthStatus defines the observed state of MaaSPlatformAuth.
type MaaSPlatformAuthStatus struct {
	// Phase represents the current phase of the platform auth configuration.
	// +kubebuilder:validation:Enum=Pending;Active;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AuthPolicyName is the name of the managed Kuadrant AuthPolicy.
	// +optional
	AuthPolicyName string `json:"authPolicyName,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="OIDC Issuer",type="string",JSONPath=".spec.externalOIDC.issuerUrl",priority=1
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MaaSPlatformAuth is the Schema for the maasplatformauths API.
// It configures platform-level authentication for the maas-api service,
// including external OIDC provider integration.
type MaaSPlatformAuth struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaaSPlatformAuthSpec   `json:"spec,omitempty"`
	Status MaaSPlatformAuthStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MaaSPlatformAuthList contains a list of MaaSPlatformAuth
type MaaSPlatformAuthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MaaSPlatformAuth `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaaSPlatformAuth{}, &MaaSPlatformAuthList{})
}
