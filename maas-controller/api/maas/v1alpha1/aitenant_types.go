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
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,JSONPath=`.status.tenantNamespace`,description="Tenant namespace"
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayRef.name`,description="Gateway name"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AITenant bootstraps one tenant slice: a tenant namespace, an existing
// network-admin-provisioned Gateway reference, the MaaS tenant config object,
// and tenant-admin RBAC.
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

	// Gateway references the network-admin-provisioned Gateway API Gateway for this tenant.
	// +kubebuilder:validation:Optional
	Gateway *AITenantGatewayRef `json:"gateway,omitempty"`

	// OIDC contains non-MaaS-specific OIDC settings for this AI Gateway tenant.
	// The controller mirrors this into the temporary Tenant config object until
	// the MaaS config CR rename lands.
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
}

// AITenantGatewayRef references the existing Gateway API Gateway for this tenant.
type AITenantGatewayRef struct {
	// Name is the Gateway name. If omitted, the AITenant name is used. The
	// namespace comes from controller configuration and is reported in status.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	Name string `json:"name,omitempty"`
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
	// +kubebuilder:validation:Enum=Pending;Active;Failed
	Phase string `json:"phase,omitempty"`

	// TenantNamespace is the reconciled tenant namespace.
	// +kubebuilder:validation:Optional
	TenantNamespace string `json:"tenantNamespace,omitempty"`

	// GatewayRef is the resolved existing Gateway reference.
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
