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
	netwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConfigKind is the API kind for the cluster-scoped MaaS platform anchor.
	ConfigKind = "Config"
	// ConfigInstanceName is the singleton resource name enforced by the API.
	ConfigInstanceName = "default"

	// ConfigConditionTenantsHealthy is the condition type set on Config.Status
	// to report the aggregate health of all AITenant CRs across the cluster.
	// It follows the ADR ODH-ADR-MS-0003 three-state model (Healthy/Degraded/Blocked).
	ConfigConditionTenantsHealthy = "TenantsHealthy"
)

// MaaSAPIEgressNetworkPolicyMode controls maas-controller reconciliation of
// maas-api-egress-restrict.
// +kubebuilder:validation:Enum=Managed;Disabled
type MaaSAPIEgressNetworkPolicyMode string

const (
	// MaaSAPIEgressNetworkPolicyManaged applies maas-api-egress-restrict (default).
	MaaSAPIEgressNetworkPolicyManaged MaaSAPIEgressNetworkPolicyMode = "Managed"
	// MaaSAPIEgressNetworkPolicyDisabled stops reconciling maas-api-egress-restrict
	// so customers can manage egress NetworkPolicies themselves.
	MaaSAPIEgressNetworkPolicyDisabled MaaSAPIEgressNetworkPolicyMode = "Disabled"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=maasconfig
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="Config name must be default"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
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
// +kubebuilder:validation:XValidation:rule="!has(self.networkPolicyEgressRules) || self.networkPolicyEgressRules.all(r, ((has(r.to) && size(r.to) > 0) && (has(r.ports) && size(r.ports) > 0)) || ((!has(r.to) || size(r.to) == 0) && has(r.ports) && size(r.ports) == 2 && r.ports.exists(p, p.port == 443 && p.protocol == 'TCP') && r.ports.exists(p, p.port == 6443 && p.protocol == 'TCP') && r.ports.all(p, (p.port == 443 || p.port == 6443) && p.protocol == 'TCP')))",message="each networkPolicyEgressRules entry must specify both destinations and ports, except the Kubernetes API port-only rule (TCP 443 and 6443)"
// +kubebuilder:validation:XValidation:rule="!has(self.networkPolicyAdditionalEgressRules) || self.networkPolicyAdditionalEgressRules.all(r, (has(r.ports) && size(r.ports) > 0) || (has(r.to) && size(r.to) > 0))",message="each networkPolicyAdditionalEgressRules entry must specify at least one port or one destination"
// +kubebuilder:validation:XValidation:rule="!has(self.maasApiEgressNetworkPolicy) || self.maasApiEgressNetworkPolicy != 'Disabled' || ((!has(self.networkPolicyEgressRules) || size(self.networkPolicyEgressRules) == 0) && (!has(self.networkPolicyAdditionalEgressRules) || size(self.networkPolicyAdditionalEgressRules) == 0))",message="networkPolicyEgressRules and networkPolicyAdditionalEgressRules must be unset when maasApiEgressNetworkPolicy is Disabled"
type ConfigSpec struct {
	// LimitadorScrapeInterval defines the scrape interval for Limitador metrics in the ServiceMonitor.
	// Defaults to "30s" if not specified.
	// +optional
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	// +kubebuilder:validation:MaxLength=16
	LimitadorScrapeInterval string `json:"limitadorScrapeInterval,omitempty"`

	// UsageLogging enables cluster-wide per-request structured OTel access logging
	// for usage tracking (token counts, identity, model). When enabled, the
	// controller deploys an EnvoyFilter on the shared gateway that emits
	// structured usage logs via OTel Access Log Service.
	// Enabling this logs identity attributes (user_id, key_name, groups,
	// subscription) per request — ensure GDPR/privacy compliance before enabling.
	// +kubebuilder:default=false
	// +kubebuilder:validation:Optional
	UsageLogging *bool `json:"usageLogging,omitempty"`

	// MaasAPIEgressNetworkPolicy controls whether maas-controller reconciles
	// maas-api-egress-restrict. Managed (default) applies the operand egress policy;
	// Disabled deletes it so customers can manage egress NetworkPolicies themselves.
	// +optional
	// +kubebuilder:validation:Enum=Managed;Disabled
	// +kubebuilder:default=Managed
	MaasAPIEgressNetworkPolicy MaaSAPIEgressNetworkPolicyMode `json:"maasApiEgressNetworkPolicy,omitempty"`

	// NetworkPolicyEgressRules, when non-empty, replaces the default egress block of
	// maas-api-egress-restrict. The caller must include DNS, Kubernetes API, database,
	// and any other required destinations. When empty, the operator applies DNS, API
	// ports, and allow-all egress for customer-provided dependencies.
	// Rules from NetworkPolicyAdditionalEgressRules are appended after this list.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	NetworkPolicyEgressRules []netwv1.NetworkPolicyEgressRule `json:"networkPolicyEgressRules,omitempty"`

	// NetworkPolicyAdditionalEgressRules specifies egress rules appended after the
	// base egress block (either defaults or NetworkPolicyEgressRules). Use for
	// non-standard ports or extra database peers when retaining default allow-all.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	NetworkPolicyAdditionalEgressRules []netwv1.NetworkPolicyEgressRule `json:"networkPolicyAdditionalEgressRules,omitempty"`
}

// ConfigStatus defines the observed state of Config.
type ConfigStatus struct {
	// Conditions represent the latest available observations of the MaaS platform state.
	// The Ready condition aggregates status from the default AITenant and MaasTenantConfig
	// so that the platform operator (DSC) can report configuration issues without watching
	// MaaS operands directly.
	// The TenantsHealthy condition aggregates the Ready state of all AITenant CRs into a
	// three-state model (Healthy/Degraded/Blocked) per ADR ODH-ADR-MS-0003.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ConfigList contains a list of Config.
type ConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Config `json:"items"`
}

func init() {
	register(&Config{}, &ConfigList{})
}
