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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// AITenantKind is the API kind for tenant bootstrap.
	AITenantKind = "AITenant"

	// AITenantConditionReady indicates whether the tenant bootstrap resources are reconciled.
	AITenantConditionReady = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ait
// +kubebuilder:validation:XValidation:rule="self.spec.tenantNamespace.name == oldSelf.spec.tenantNamespace.name",message="spec.tenantNamespace.name is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.tls) || self.spec.domain != ''",message="spec.tls requires spec.domain to be set"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,JSONPath=`.status.tenantNamespace`,description="Tenant namespace"
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayRef.name`,description="Gateway name"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AITenant bootstraps one tenant slice: a dedicated Gateway, a tenant namespace,
// the MaaS tenant config object, and tenant-admin RBAC.
type AITenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AITenantSpec   `json:"spec,omitempty"`
	Status AITenantStatus `json:"status,omitempty"`
}

// AITenantSpec defines the tenant bootstrap contract.
type AITenantSpec struct {
	// TenantNamespace identifies the namespace where tenant administrators manage MaaS objects.
	TenantNamespace AITenantTenantNamespace `json:"tenantNamespace"`

	// Gateway is the Gateway API template reconciled for this tenant.
	// +kubebuilder:validation:Optional
	Gateway *AITenantGatewayTemplate `json:"gateway,omitempty"`

	// Domain is the tenant hostname used for data-plane routing.
	// When set together with TLS, the controller creates an HTTPS listener on port 443.
	// When set without TLS, the controller creates an HTTP listener on port 80.
	// When omitted, the controller creates a default HTTP listener on port 80 without a hostname.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	Domain string `json:"domain,omitempty"`

	// TLS configures the TLS certificate for the tenant Gateway HTTPS listener.
	// Only effective when Domain is also set.
	// +kubebuilder:validation:Optional
	TLS *AITenantTLS `json:"tls,omitempty"`

	// OIDC contains non-MaaS-specific OIDC settings for this AI Gateway tenant.
	// The current controller mirrors this into the temporary Tenant config object
	// until the MaaS config CR rename lands.
	// +kubebuilder:validation:Optional
	OIDC *TenantExternalOIDCConfig `json:"oidc,omitempty"`

	// RBAC configures tenant-admin access to the tenant namespace and this AITenant object.
	// +kubebuilder:validation:Optional
	RBAC *AITenantRBACConfig `json:"rbac,omitempty"`
}

// AITenantTenantNamespace defines how the tenant namespace is handled.
type AITenantTenantNamespace struct {
	// Name is the namespace where tenant-scoped MaaS objects are created.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Create controls whether the controller creates the namespace if missing.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=true
	Create *bool `json:"create,omitempty"`

	// CleanupOnDelete deletes the namespace during AITenant deletion only when
	// the namespace is controller-created and still labeled for this AITenant.
	// Defaults to false to avoid deleting tenant data by surprise.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	CleanupOnDelete *bool `json:"cleanupOnDelete,omitempty"`
}

// AITenantGatewayTemplate describes the Gateway API resource to create.
type AITenantGatewayTemplate struct {
	// Name is the Gateway name. If omitted, the AITenant name is used.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	Name string `json:"name,omitempty"`

	// GatewayClassName is the GatewayClass used by the tenant Gateway.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="openshift-default"
	// +kubebuilder:validation:MaxLength=253
	GatewayClassName string `json:"gatewayClassName,omitempty"`
}

// AITenantTLS configures the TLS certificate for the tenant Gateway.
type AITenantTLS struct {
	// CertificateRef names a Secret containing the TLS certificate.
	CertificateRef AITenantCertificateRef `json:"certificateRef"`
}

// AITenantCertificateRef references a TLS Secret.
type AITenantCertificateRef struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AITenantRBACConfig defines tenant-admin subjects.
type AITenantRBACConfig struct {
	// Admins are bound to tenant-admin roles for this AITenant and tenant namespace.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	Admins []AITenantRBACSubject `json:"admins,omitempty"`
}

// AITenantRBACSubject mirrors RBAC Subject for the supported tenant-admin cases.
type AITenantRBACSubject struct {
	// Kind is the RBAC subject kind.
	// +kubebuilder:validation:Enum=User;Group;ServiceAccount
	Kind string `json:"kind"`

	// Name is the subject name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is required only for ServiceAccount subjects.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// AITenantStatus defines the observed tenant bootstrap state.
type AITenantStatus struct {
	// Phase is a high-level lifecycle phase for the tenant bootstrap.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// TenantNamespace is the reconciled tenant namespace.
	// +kubebuilder:validation:Optional
	TenantNamespace string `json:"tenantNamespace,omitempty"`

	// GatewayRef is the reconciled Gateway reference.
	// +kubebuilder:validation:Optional
	GatewayRef TenantGatewayRef `json:"gatewayRef,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// AITenantList contains a list of AITenant.
type AITenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AITenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AITenant{}, &AITenantList{})
}
