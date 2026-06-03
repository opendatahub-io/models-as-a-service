// Package tenantreconcile implements the Tenant platform reconcile pipeline
// (initialize → dependencies → prerequisites → gateway → params → kustomize → post-render → apply → deployment status).
// The pipeline stages mirror the ODH operator's component deploy pattern; the Tenant CR is the
// runtime object that drives this lifecycle (DSC.modelsAsService controls enablement only).
package tenantreconcile

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// OptionalAPIGroups lists API groups whose CRDs are installed by optional platform
// components (e.g. COO for Perses). Resources in these groups are skipped gracefully
// when their CRDs are not yet registered, instead of failing the Tenant reconcile.
// The CRD watch in the controller re-triggers reconcile once the CRDs appear.
var OptionalAPIGroups = map[string]bool{
	"perses.dev": true, // Cluster Observability Operator (COO) — Perses dashboards and datasources
}

// isOptionalAPIGroup returns true when missing CRDs for the given group should not
// fail the reconcile (i.e. the dependency is installed by an optional operator).
func isOptionalAPIGroup(group string) bool {
	return OptionalAPIGroups[group]
}

const (
	// AnnotationManaged is the opt-out annotation key used by the ODH operator and MaaS
	// controller. Resources annotated with this key set to "false" are skipped on both
	// the write (create/update) and delete paths. This is the single source of truth;
	// other packages that need the same key must reference this constant.
	AnnotationManaged = "opendatahub.io/managed"

	// ComponentName is the ODH component label key suffix (app.opendatahub.io/<name>).
	// This is the DSC component identifier, not a standalone CR kind.
	ComponentName = "modelsasservice"

	LabelODHAppPrefix    = "app.opendatahub.io"
	LabelK8sPartOf       = "app.kubernetes.io/part-of"
	LabelTenantName      = "maas.opendatahub.io/tenant-name"
	LabelTenantNamespace = "maas.opendatahub.io/tenant-namespace"

	DefaultMaaSAPIImage            = "quay.io/opendatahub/maas-api:latest"
	DefaultPayloadProcessingImage  = "quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable"
	DefaultMaaSAPIKeyCleanupImage  = "registry.redhat.io/ubi9/ubi-minimal:9.7"
	DefaultAPIKeyMaxExpirationDays = "90"

	GatewayDefaultAuthPolicyName                  = "gateway-default-auth"
	GatewayTokenRateLimitDefaultDenyPolicyName    = "gateway-default-deny"
	MaaSAPIAuthPolicyName                         = "maas-api-auth-policy"
	MaaSAPIRouteName                              = "maas-api-route"
	MaaSAPIKeyCleanupCronJobName                  = "maas-api-key-cleanup" //nolint:gosec // Kubernetes resource name, not a credential
	GatewayDestinationRuleName                    = "maas-api-backend-tls"
	TelemetryPolicyName                           = "maas-telemetry"
	IstioTelemetryName                            = "latency-per-subscription"
	MaaSAPIDeploymentName                         = "maas-api"
	PayloadProcessingName                         = "payload-processing"
	PayloadPreProcessingName                      = "payload-pre-processing"
	PayloadProcessingPluginsConfigMapName         = "payload-processing-plugins"
	PayloadProcessingReaderClusterRoleBindingName = "payload-processing-reader"
	// MaaSControllerDeploymentName matches deployment/base/maas-controller/manager/manager.yaml.
	MaaSControllerDeploymentName = "maas-controller"
	MaaSDBSecretName             = "maas-db-config" //nolint:gosec // secret name reference, not a credential
	MaaSDBSecretKey              = "DB_CONNECTION_URL"

	MonitoringNamespace         = "openshift-monitoring"
	ClusterMonitoringConfigName = "cluster-monitoring-config"

	// Condition types aligned with ODH internal/controller/status for DSC aggregation parity.
	ConditionDependenciesAvailable      = "DependenciesAvailable"
	ConditionMaaSPrerequisitesAvailable = "MaaSPrerequisitesAvailable"
	ConditionDeploymentsAvailable       = "DeploymentsAvailable"
	ConditionTypeDegraded               = "Degraded"
	ReadyConditionType                  = "Ready"
)

// GVKs used for post-render and readiness (mirrors opendatahub-operator/pkg/cluster/gvk selections).
var (
	GVKDeployment           = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	GVKHTTPRoute            = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	GVKCronJob              = schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}
	GVKAuthPolicy           = schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}
	GVKTokenRateLimitPolicy = schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"}
	GVKDestinationRule      = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"}
	GVKTelemetryPolicy      = schema.GroupVersionKind{Group: "extensions.kuadrant.io", Version: "v1alpha1", Kind: "TelemetryPolicy"}
	GVKEnvoyFilter          = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"}
	GVKIstioTelemetry       = schema.GroupVersionKind{Group: "telemetry.istio.io", Version: "v1", Kind: "Telemetry"}
	GVKAuthConfig           = schema.GroupVersionKind{Group: "authorino.kuadrant.io", Version: "v1beta3", Kind: "AuthConfig"}
	GVKAuthorino            = schema.GroupVersionKind{Group: "operator.authorino.kuadrant.io", Version: "v1beta1", Kind: "Authorino"}
	GVKService              = schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	GVKServiceAccount       = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}
	GVKConfigMap            = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	GVKClusterRoleBinding   = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}
	GVKPersesDashboard      = schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDashboard"}
	GVKPersesDatasource     = schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDatasource"}
)
