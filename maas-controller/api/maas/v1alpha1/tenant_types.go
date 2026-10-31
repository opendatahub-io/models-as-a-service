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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// TenantKind is the deprecated API kind for the legacy tenant config singleton.
	TenantKind = "Tenant"
	// TenantInstanceName is the deprecated singleton resource name enforced by the API.
	TenantInstanceName = "default-tenant"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-tenant'",message="Tenant name must be default-tenant"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is the deprecated namespace-scoped API used before the MaasTenantConfig
// rename. It remains registered during the migration window so existing clusters
// can copy MaaS-owned settings into MaasTenantConfig/default-tenant and preserve
// legacy Gateway/OIDC values for AITenant bootstrap.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// TenantSpec defines the desired state of Tenant.
type TenantSpec struct {
	// GatewayRef specifies which Gateway (Gateway API) to use for exposing model endpoints
	// for legacy/unmanaged Tenant resources. For AITenant-managed tenants, AITenant
	// owns this platform context and the controller ignores this field.
	// +kubebuilder:validation:Optional
	GatewayRef TenantGatewayRef `json:"gatewayRef,omitempty"`

	// APIKeys contains configuration for API key management.
	// +kubebuilder:validation:Optional
	APIKeys *TenantAPIKeysConfig `json:"apiKeys,omitempty"`

	// ExternalOIDC configures an external OIDC identity provider for legacy/unmanaged
	// Tenant resources. For AITenant-managed tenants, use AITenant.spec.oidc.
	// +kubebuilder:validation:Optional
	ExternalOIDC *TenantExternalOIDCConfig `json:"externalOIDC,omitempty"`

	// Telemetry contains configuration for telemetry and metrics collection.
	// +kubebuilder:validation:Optional
	Telemetry *TenantTelemetryConfig `json:"telemetry,omitempty"`

	// MaasAPI defines scaling and resource configuration for maas-api pods.
	// +kubebuilder:validation:Optional
	MaasAPI *TenantMaasAPIConfig `json:"maasApi,omitempty"`

	// PayloadProcessing defines configuration for payload-processing pods
	// (scaling/resources and ExtProc server.max_body_bytes).
	// +kubebuilder:validation:Optional
	PayloadProcessing *TenantPayloadProcessingConfig `json:"payloadProcessing,omitempty"`

	// PayloadPreProcessing defines scaling configuration for payload-pre-processing pods.
	// +kubebuilder:validation:Optional
	PayloadPreProcessing *TenantPayloadProcessingConfig `json:"payloadPreProcessing,omitempty"`
}

// TenantExternalOIDCConfig defines the external OIDC provider settings.
// +kubebuilder:validation:XValidation:rule="has(self.accessTokenPurposeClaim) == has(self.accessTokenPurpose)",message="accessTokenPurposeClaim and accessTokenPurpose must be set together"
// +kubebuilder:validation:XValidation:rule="(has(self.cookieExpire) ? duration(self.cookieExpire) : duration('6h')) > (has(self.refreshInterval) ? duration(self.refreshInterval) : duration('4m'))",message="cookieExpire must be longer than refreshInterval"
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

	// Audience is the exact OIDC audience required in the token `aud` claim.
	// It must be set for delegated Grid administration.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^\S+$`
	Audience string `json:"audience,omitempty"`

	// AccessTokenPurposeClaim is the simple token claim name that carries the
	// access-token purpose, such as token_use. It and AccessTokenPurpose must be
	// set together and are required for delegated Grid administration.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	AccessTokenPurposeClaim string `json:"accessTokenPurposeClaim,omitempty"`

	// AccessTokenPurpose is the exact value required in AccessTokenPurposeClaim.
	// It and AccessTokenPurposeClaim must be set together and are required for
	// delegated Grid administration.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^\S+$`
	AccessTokenPurpose string `json:"accessTokenPurpose,omitempty"`

	// TTL is the JWKS cache duration in seconds.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=30
	TTL int `json:"ttl,omitempty"`

	// ClientSecretRef names the Secret in the tenant namespace that holds the
	// OIDC client secret for the consumer portal auth proxy. The tenant creates
	// the Secret; the platform copies it next to the proxy and keeps the copy
	// in sync.
	// +kubebuilder:validation:Optional
	ClientSecretRef *OIDCClientSecretReference `json:"clientSecretRef,omitempty"`

	// RefreshInterval is how often the consumer portal auth proxy refreshes the
	// session's access token. It must be shorter than the identity provider's
	// access token lifetime. Defaults to 4m when unset.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="refreshInterval must be a positive duration"
	RefreshInterval *metav1.Duration `json:"refreshInterval,omitempty"`

	// CookieExpire is how long a consumer portal session lasts before the auth
	// proxy signs the user out. It must be longer than RefreshInterval.
	// Defaults to 6h when unset.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="cookieExpire must be a positive duration"
	CookieExpire *metav1.Duration `json:"cookieExpire,omitempty"`
}

// OIDCClientSecretReference selects a key of a Secret in the tenant namespace.
type OIDCClientSecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key in the Secret's data that holds the client secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// TenantTelemetryConfig defines configuration for telemetry collection.
type TenantTelemetryConfig struct {
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	Enabled *bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:Optional
	Metrics *TenantMetricsConfig `json:"metrics,omitempty"`

	// +kubebuilder:validation:Optional
	Logs *TenantLogsConfig `json:"logs,omitempty"`
}

// TenantMetricsConfig defines optional metric dimensions.
type TenantMetricsConfig struct {
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	CaptureOrganization *bool `json:"captureOrganization,omitempty"`

	// CaptureUser adds a "user" dimension to telemetry metrics containing
	// the authenticated user ID. Defaults to false. Enabling this may
	// have GDPR / privacy implications — ensure compliance before use.
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

// TenantLogsConfig defines optional usage-log identity dimensions.
type TenantLogsConfig struct {
	// CaptureUser includes the authenticated user ID as user_id on usage logs.
	// Defaults to false. Enabling this may have GDPR / privacy implications —
	// ensure compliance before use. Independent of metrics.captureUser.
	// +kubebuilder:default=false
	// +kubebuilder:validation:Optional
	CaptureUser *bool `json:"captureUser,omitempty"`

	// CaptureOrganization includes the subscription organization ID as
	// organization_id on usage logs. Defaults to true. Independent of
	// metrics.captureOrganization.
	// +kubebuilder:default=true
	// +kubebuilder:validation:Optional
	CaptureOrganization *bool `json:"captureOrganization,omitempty"`
}

// TenantAPIKeysConfig defines configuration options for API key management.
type TenantAPIKeysConfig struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	MaxExpirationDays *int32 `json:"maxExpirationDays,omitempty"`

	// DeletionRetentionDays controls how long lifecycle-invalidated API keys
	// remain soft-deleted before physical deletion. The default is 90 days.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=106751
	DeletionRetentionDays *int32 `json:"deletionRetentionDays,omitempty"`
}

// TenantMaasAPIConfig defines scaling and resource configuration for maas-api pods.
type TenantMaasAPIConfig struct {
	// Replicas overrides the maas-api Deployment replica count.
	// Spec-based replicas take precedence over the maas.opendatahub.io/maas-api-replicas annotation.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources overrides the resource requests and limits for the maas-api container.
	// When set, replaces the entire resource block (full replacement, not merge).
	// Resource claims are not supported.
	// +kubebuilder:validation:Optional
	Resources *TenantResourceRequirements `json:"resources,omitempty"`
}

// TenantPayloadProcessingConfig defines configuration for a payload-processing
// workload Deployment (payload-processing or payload-pre-processing), including
// scaling/resources and, when used under MaasTenantConfig.spec.payloadProcessing,
// the ExtProc request-body accumulation ceiling.
type TenantPayloadProcessingConfig struct {
	// Replicas overrides the Deployment replica count.
	// When Autoscaling is enabled, this value sets the HPA minReplicas floor.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas *int32 `json:"replicas,omitempty"`

	// Autoscaling enables HPA-based horizontal pod autoscaling.
	// When enabled, an HPA is created targeting the corresponding Deployment.
	// +kubebuilder:validation:Optional
	Autoscaling *TenantAutoscalingConfig `json:"autoscaling,omitempty"`

	// Resources overrides the resource requests and limits for the container.
	// When set, replaces the entire resource block (full replacement, not merge).
	// When autoscaling is enabled, both requests.cpu and requests.memory must be specified.
	// Resource claims are not supported.
	// +kubebuilder:validation:Optional
	Resources *TenantResourceRequirements `json:"resources,omitempty"`

	// MaxBodyBytes bounds ExtProc request-body accumulation (server.max_body_bytes)
	// for the Praxis payload-processing dataplane. This is ExtProc's accumulation
	// ceiling, not Envoy's HTTP buffer filter. When set under
	// MaasTenantConfig.spec.payloadProcessing, the AI Gateway controller always
	// renders server.max_body_bytes and never allow_unbounded_body. Omission
	// defaults to 10485760 (10 MiB); values above 104857600 (100 MiB) are rejected.
	// Under payloadPreProcessing the field is currently unused by the AI Gateway
	// controller. Keep the Maximum in sync with AGC MaxExtProcMaxBodyBytes.
	// +kubebuilder:default=10485760
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=104857600
	MaxBodyBytes *int64 `json:"maxBodyBytes,omitempty"`
}

// TenantResourceRequirements defines CPU and memory requests and limits for a container.
// Only requests and limits are supported; resource claims are not accepted.
type TenantResourceRequirements struct {
	// Limits defines the maximum amount of compute resources allowed.
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`

	// Requests defines the minimum amount of compute resources required.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
}

// TenantAutoscalingConfig defines HPA autoscaling parameters for IPP workloads.
type TenantAutoscalingConfig struct {
	// MaxReplicas is the upper limit for the HPA replica count.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`

	// TargetCPUUtilization is the target average CPU utilization percentage.
	// +kubebuilder:default=70
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetCPUUtilization *int32 `json:"targetCPUUtilization,omitempty"`

	// TargetMemoryUtilization is the target average memory utilization percentage.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TargetMemoryUtilization *int32 `json:"targetMemoryUtilization,omitempty"`
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

// TenantStatus defines the observed state of Tenant.
type TenantStatus struct {
	// Phase is a high-level lifecycle phase for the platform reconcile.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Pending;Active;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// Types mirror ODH modelsasservice / maas-controller status for DSC aggregation: Ready,
	// DependenciesAvailable, MaaSPrerequisitesAvailable, DeploymentsAvailable, Degraded.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList contains a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	register(&Tenant{}, &TenantList{})
}
