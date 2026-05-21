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
	// AIGatewayKind is the API kind for tenant bootstrap.
	AIGatewayKind = "AIGateway"

	// AIGatewayConditionReady indicates whether the tenant bootstrap resources are reconciled.
	AIGatewayConditionReady = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aigw
// +kubebuilder:validation:XValidation:rule="self.spec.tenantNamespace.name == oldSelf.spec.tenantNamespace.name",message="spec.tenantNamespace.name is immutable"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Tenant Namespace",type=string,JSONPath=`.status.tenantNamespace`,description="Tenant namespace"
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.status.gatewayRef.name`,description="Gateway name"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AIGateway bootstraps one tenant slice: a dedicated Gateway, a tenant namespace,
// the MaaS tenant config object, and tenant-admin RBAC.
type AIGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIGatewaySpec   `json:"spec,omitempty"`
	Status AIGatewayStatus `json:"status,omitempty"`
}

// AIGatewaySpec defines the tenant bootstrap contract.
type AIGatewaySpec struct {
	// TenantNamespace identifies the namespace where tenant administrators manage MaaS objects.
	TenantNamespace AIGatewayTenantNamespace `json:"tenantNamespace"`

	// Gateway is the Gateway API template reconciled for this tenant.
	// +kubebuilder:validation:Optional
	Gateway *AIGatewayGatewayTemplate `json:"gateway,omitempty"`

	// OIDC contains non-MaaS-specific OIDC settings for this AI Gateway.
	// The current controller mirrors this into the temporary Tenant config object
	// until the MaaS config CR rename lands.
	// +kubebuilder:validation:Optional
	OIDC *TenantExternalOIDCConfig `json:"oidc,omitempty"`

	// RBAC configures tenant-admin access to the tenant namespace and this AIGateway object.
	// +kubebuilder:validation:Optional
	RBAC *AIGatewayRBACConfig `json:"rbac,omitempty"`
}

// AIGatewayTenantNamespace defines how the tenant namespace is handled.
type AIGatewayTenantNamespace struct {
	// Name is the namespace where tenant-scoped MaaS objects are created.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Create controls whether the controller creates the namespace if missing.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=true
	Create *bool `json:"create,omitempty"`

	// CleanupOnDelete deletes the namespace during AIGateway deletion only when
	// the namespace is controller-created and still labeled for this AIGateway.
	// Defaults to false to avoid deleting tenant data by surprise.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	CleanupOnDelete *bool `json:"cleanupOnDelete,omitempty"`
}

// AIGatewayGatewayTemplate describes the Gateway API resource to create.
type AIGatewayGatewayTemplate struct {
	// Name is the Gateway name. If omitted, the AIGateway name is used.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	Name string `json:"name,omitempty"`

	// Namespace is the namespace where the Gateway is created.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
	Namespace string `json:"namespace,omitempty"`

	// GatewayClassName is the GatewayClass used by the tenant Gateway.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="openshift-default"
	// +kubebuilder:validation:MaxLength=253
	GatewayClassName string `json:"gatewayClassName,omitempty"`

	// Listeners is the set of Gateway listeners. If omitted, a single HTTP listener is created.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	Listeners []AIGatewayListener `json:"listeners,omitempty"`
}

// AIGatewayListener is the supported listener subset for the tenant Gateway template.
type AIGatewayListener struct {
	// Name is the Gateway listener name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Hostname is the optional hostname for HTTP, HTTPS, and TLS listeners.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname,omitempty"`

	// Port is the listener port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Protocol is the listener protocol.
	// +kubebuilder:validation:Enum=HTTP;HTTPS;TLS;TCP;UDP
	Protocol string `json:"protocol"`

	// TLS configures certificate references for HTTPS or TLS listeners.
	// +kubebuilder:validation:Optional
	TLS *AIGatewayListenerTLS `json:"tls,omitempty"`

	// AllowedRoutes controls which route namespaces may bind to the listener.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=All
	// +kubebuilder:validation:Enum=All;Same
	AllowedRoutesFrom string `json:"allowedRoutesFrom,omitempty"`
}

// AIGatewayListenerTLS contains the supported Gateway TLS fields.
type AIGatewayListenerTLS struct {
	// Mode controls TLS termination behavior.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=Terminate
	// +kubebuilder:validation:Enum=Terminate;Passthrough
	Mode string `json:"mode,omitempty"`

	// CertificateRefs names Secret certificates for terminating TLS.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=16
	CertificateRefs []AIGatewayCertificateRef `json:"certificateRefs,omitempty"`
}

// AIGatewayCertificateRef references a TLS Secret.
type AIGatewayCertificateRef struct {
	// Name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// AIGatewayRBACConfig defines tenant-admin subjects.
type AIGatewayRBACConfig struct {
	// Admins are bound to tenant-admin roles for this AIGateway and tenant namespace.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	Admins []AIGatewayRBACSubject `json:"admins,omitempty"`
}

// AIGatewayRBACSubject mirrors RBAC Subject for the supported tenant-admin cases.
type AIGatewayRBACSubject struct {
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

// AIGatewayStatus defines the observed tenant bootstrap state.
type AIGatewayStatus struct {
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

// AIGatewayList contains a list of AIGateway.
type AIGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AIGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AIGateway{}, &AIGatewayList{})
}
