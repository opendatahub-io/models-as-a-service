// Package tenantreconcile implements the Tenant platform reconcile pipeline
// (initialize → dependencies → prerequisites → gateway → params → kustomize → post-render → apply → deployment status).
// The pipeline stages mirror the ODH operator's component deploy pattern; the MaasTenantConfig CR is the
// runtime object that drives this lifecycle (DSC.modelsAsService controls enablement only).
package tenantreconcile

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

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

	// LabelAIGatewayTenant is the ADR-defined tenant marker on tenant admin namespaces.
	LabelAIGatewayTenant = "ai-gateway.opendatahub.io/tenant"

	// LabelManagedByAITenant is set to "true" on namespaces created/adopted
	// by the AITenant reconciler (S11). The subscription/auth-policy controllers
	// use this label to discover tenant namespaces dynamically.
	LabelManagedByAITenant = "maas.opendatahub.io/managed-by-aitenant"

	// DefaultAITenantNamespace is the default namespace where AITenant CRs are created.
	DefaultAITenantNamespace = "ai-tenants"

	// DefaultAITenantName is the AITenant that represents the legacy/default
	// models-as-a-service installation during single-tenant to multi-tenant migration.
	DefaultAITenantName = "models-as-a-service"

	// DefaultInfraNamespace is the fallback infrastructure namespace when --infra-namespace flag is not specified.
	// AUTO = automatically derive from controller namespace (namespace separation enabled by default).
	// Note: PostgreSQL can be external - only maas-api and the connection secret deploy here.
	// Production deployments use this default. ROSA deployments must override to "" (empty) to disable separation.
	DefaultInfraNamespace = "AUTO"

	DefaultMaaSAPIImage            = "quay.io/opendatahub/maas-api:latest"
	DefaultPayloadProcessingImage  = "quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable"
	DefaultMaaSAPIKeyCleanupImage  = "registry.redhat.io/ubi9/ubi-minimal:9.7"
	DefaultAPIKeyMaxExpirationDays = "90"

	// Resource name base constants for multi-tenant resources.
	// These are used with tenant identifiers to create unique resource names per tenant.
	// For legacy/default tenant (empty tenantID), these values are used as-is.
	baseGatewayDefaultAuthPolicyName               = "gateway-default-auth"
	baseGatewayTokenRateLimitDefaultDenyPolicyName = "gateway-default-deny"
	baseMaaSAPIAuthPolicyName                      = "maas-api-auth-policy"
	baseMaaSAPIRouteName                           = "maas-api-route"
	baseMaaSAPIKeyCleanupCronJobName               = "maas-api-key-cleanup" //nolint:gosec // Kubernetes resource name, not a credential
	baseGatewayDestinationRuleName                 = "maas-api-backend-tls"
	baseTelemetryPolicyName                        = "maas-telemetry"
	baseIstioTelemetryName                         = "latency-per-subscription"
	baseMaaSAPIDeploymentName                      = "maas-api"
	baseMaaSAPIServiceName                         = "maas-api"
	baseMaaSAPIKeyCleanupScriptConfigMapName       = "maas-api-key-cleanup-script" //nolint:gosec // Kubernetes resource name, not a credential
	baseMaaSAPIDeploymentNSNetworkPolicyName       = "maas-api-allow-deployment-ns"

	// Non-tenant-specific resource names (shared infrastructure)
	PayloadProcessingName                         = "payload-processing"
	PayloadPreProcessingName                      = "payload-pre-processing"
	PayloadProcessingPluginsConfigMapName         = "payload-processing-plugins"
	PayloadProcessingReaderClusterRoleBindingName = "payload-processing-reader"
	// MaaSControllerDeploymentName matches deployment/base/maas-controller/manager/manager.yaml.
	MaaSControllerDeploymentName = "maas-controller"
	MaaSDBSecretName             = "maas-db-config" //nolint:gosec // secret name reference, not a credential
	MaaSDBSecretKey              = "DB_CONNECTION_URL"

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
	GVKNetworkPolicy        = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
	GVKPersesDashboard      = schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDashboard"}
	GVKPersesDatasource     = schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDatasource"}
)

// Resource naming functions for multi-tenant deployment.
// Each function accepts a tenantID string and returns the appropriate resource name.
//
// Tenant identifier behavior:
// - Empty string ("") = default/legacy tenant (preserves existing names for backward compatibility)
//   TODO: When database tenant_id filtering is implemented in maas-api, migrate empty string
//   to "models-as-a-service" for consistency with ADR and database tenant_id column.
// - Non-empty string = AITenant-managed tenant (e.g., "redteam" from AITenant.metadata.name)
//
// Resource naming pattern:
// - Default tenant (tenantID=""): Returns base name as-is (e.g., "maas-api")
// - AITenant tenant (tenantID="redteam"): Returns "{base}-{tenantID}" (e.g., "maas-api-redteam")
//
// Name length constraints:
// - Kubernetes resources (Service, ConfigMap, etc.) have a 63-character limit
// - Longest base name is "maas-api-auth-policy" (21 chars)
// - Max tenant name: 63 - 21 - 1 (separator) = 41 chars
// - AITenant CRD enforces this 41-char limit via XValidation

func resourceNameForTenant(baseName, tenantID string) string {
	if tenantID == "" {
		return baseName
	}
	return baseName + "-" + tenantID
}

func GatewayDefaultAuthPolicyName(tenantID string) string {
	return resourceNameForTenant(baseGatewayDefaultAuthPolicyName, tenantID)
}

func GatewayTokenRateLimitDefaultDenyPolicyName(tenantID string) string {
	return resourceNameForTenant(baseGatewayTokenRateLimitDefaultDenyPolicyName, tenantID)
}

func MaaSAPIAuthPolicyName(tenantID string) string {
	return resourceNameForTenant(baseMaaSAPIAuthPolicyName, tenantID)
}

func MaaSAPIRouteName(tenantID string) string {
	return resourceNameForTenant(baseMaaSAPIRouteName, tenantID)
}

func MaaSAPIKeyCleanupCronJobName(tenantID string) string {
	return resourceNameForTenant(baseMaaSAPIKeyCleanupCronJobName, tenantID)
}

func GatewayDestinationRuleName(tenantID string) string {
	return resourceNameForTenant(baseGatewayDestinationRuleName, tenantID)
}

func TelemetryPolicyName(tenantID string) string {
	return resourceNameForTenant(baseTelemetryPolicyName, tenantID)
}

func IstioTelemetryName(tenantID string) string {
	return resourceNameForTenant(baseIstioTelemetryName, tenantID)
}

func MaaSAPIDeploymentName(tenantID string) string {
	name := resourceNameForTenant(baseMaaSAPIDeploymentName, tenantID)
	ctrl.Log.WithName("MaaSAPIDeploymentName").Info("Generated deployment name",
		"tenantID", tenantID,
		"deploymentName", name)
	return name
}

func MaaSAPIServiceName(tenantID string) string {
	return resourceNameForTenant(baseMaaSAPIServiceName, tenantID)
}

// TenantIdentifierFor extracts the tenant identifier from a tenant config object.
//
// Returns:
//   - Empty string ("") for the default/legacy config, whether unlabeled or managed
//     by AITenant/models-as-a-service (preserves legacy resource names)
//   - Tenant name (e.g., "redteam") for non-default AITenant-managed tenants
//   - Error if LabelManagedByAITenant is true but LabelTenantName is missing
//
// The tenant identifier is used to generate unique per-tenant resource names.
func TenantIdentifierFor(obj client.Object) (string, error) {
	if isNilObject(obj) {
		return "", nil
	}

	labels := obj.GetLabels()
	log := ctrl.Log.WithName("TenantIdentifierFor").WithValues(
		"tenantConfig", obj.GetNamespace()+"/"+obj.GetName(),
		"labels", labels,
	)

	if labels != nil && labels[LabelManagedByAITenant] == "true" {
		tenantName := labels[LabelTenantName]
		if tenantName == "" {
			log.Error(nil, "AITenant-managed tenant config is missing LabelTenantName",
				"managedByLabel", LabelManagedByAITenant,
				"tenantNameLabel", LabelTenantName)
			return "", fmt.Errorf("tenant %s/%s has %s=true but %s is missing or empty",
				obj.GetNamespace(), obj.GetName(), LabelManagedByAITenant, LabelTenantName)
		}
		if tenantName == DefaultAITenantName {
			if obj.GetName() != maasv1alpha1.MaasTenantConfigInstanceName {
				return "", fmt.Errorf("tenant config %s/%s has %s=%q but is not the default tenant config resource %q",
					obj.GetNamespace(), obj.GetName(), LabelTenantName, DefaultAITenantName, maasv1alpha1.MaasTenantConfigInstanceName)
			}
			if labels[LabelTenantNamespace] == "" || labels[LabelTenantNamespace] != obj.GetNamespace() {
				return "", fmt.Errorf("tenant config %s/%s has %s=%q but %s must match the tenant config namespace",
					obj.GetNamespace(), obj.GetName(), LabelTenantName, DefaultAITenantName, LabelTenantNamespace)
			}
			log.Info("Using default AITenant legacy resource identifier (empty string)",
				"tenantName", tenantName)
			return "", nil
		}
		log.Info("Resolved tenant identifier from AITenant label", "tenantIdentifier", tenantName)
		return tenantName, nil
	}

	log.Info("Using legacy/default tenant identifier (empty string)", "reason", "no AITenant label")
	return "", nil
}

// TenantNameFor returns the tenant name used for database queries, AuthPolicy headers,
// and maas-api TENANT_NAME environment variable. This is distinct from TenantIdentifierFor,
// which is used for resource naming.
//
// Returns:
//   - "models-as-a-service" for the default tenant (matches DB default and TENANT_NAME env var)
//   - The tenant name for AITenant-managed tenants
func TenantNameFor(obj client.Object) (string, error) {
	if isNilObject(obj) {
		return "models-as-a-service", nil
	}

	labels := obj.GetLabels()
	if labels != nil && labels[LabelManagedByAITenant] == "true" {
		tenantName := labels[LabelTenantName]
		if tenantName == "" {
			return "", fmt.Errorf("tenant %s/%s has %s=true but %s is missing or empty",
				obj.GetNamespace(), obj.GetName(), LabelManagedByAITenant, LabelTenantName)
		}
		return tenantName, nil
	}

	// Default tenant uses "models-as-a-service" for database/headers
	return "models-as-a-service", nil
}

func isNilObject(obj client.Object) bool {
	if obj == nil {
		return true
	}
	value := reflect.ValueOf(obj)
	return value.Kind() == reflect.Pointer && value.IsNil()
}
