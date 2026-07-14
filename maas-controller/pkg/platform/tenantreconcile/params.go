package tenantreconcile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// PlatformParams holds resolved runtime values for PostRender patching.
type PlatformParams struct {
	AppNamespace          string
	GatewayNamespace      string
	GatewayName           string
	ClusterAudience       string
	SubscriptionNamespace string
	ExternalOIDC          *maasv1alpha1.TenantExternalOIDCConfig

	// TenantIdentifier is the tenant name used for per-tenant resource naming.
	// Empty string ("") for default/legacy tenant, non-empty (e.g., "redteam") for AITenant-managed tenants.
	TenantIdentifier string

	MaaSAPIImage           string
	PayloadProcessingImage string
	MaaSAPIKeyCleanupImage string

	APIKeyMaxExpirationDays string
}

// BuildPlatformParams resolves all runtime parameters from the tenant config object,
// platform context, cluster state, and RELATED_IMAGE_* env vars. No disk I/O.
func BuildPlatformParams(tenant client.Object, platformContext PlatformContext, appNamespace, clusterAudience string, log logr.Logger) (PlatformParams, error) {
	tenantID, err := TenantIdentifierFor(tenant)
	if err != nil {
		return PlatformParams{}, fmt.Errorf("resolve tenant identifier: %w", err)
	}

	params := PlatformParams{
		AppNamespace:            appNamespace,
		GatewayNamespace:        platformContext.GatewayRef.Namespace,
		GatewayName:             platformContext.GatewayRef.Name,
		ClusterAudience:         clusterAudience,
		SubscriptionNamespace:   tenant.GetNamespace(),
		ExternalOIDC:            platformContext.ExternalOIDC.DeepCopy(),
		TenantIdentifier:        tenantID,
		MaaSAPIImage:            firstNonEmpty(os.Getenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE"), DefaultMaaSAPIImage),
		PayloadProcessingImage:  firstNonEmpty(os.Getenv("RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE"), DefaultPayloadProcessingImage),
		MaaSAPIKeyCleanupImage:  firstNonEmpty(os.Getenv("RELATED_IMAGE_UBI_MINIMAL_IMAGE"), DefaultMaaSAPIKeyCleanupImage),
		APIKeyMaxExpirationDays: resolveAPIKeyMaxExpirationDays(tenant),
	}

	log.Info("Built platform params",
		"tenant", tenant.GetNamespace()+"/"+tenant.GetName(),
		"tenantID", tenantID,
		"subscriptionNamespace", params.SubscriptionNamespace,
		"gatewayName", params.GatewayName)

	return params, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func resolveAPIKeyMaxExpirationDays(tenant client.Object) string {
	cfg := apiKeysConfigFor(tenant)
	if cfg != nil && cfg.MaxExpirationDays != nil {
		return strconv.FormatInt(int64(*cfg.MaxExpirationDays), 10)
	}
	return DefaultAPIKeyMaxExpirationDays
}

func apiKeysConfigFor(tenant client.Object) *maasv1alpha1.TenantAPIKeysConfig {
	switch t := tenant.(type) {
	case *maasv1alpha1.MaasTenantConfig:
		return t.Spec.APIKeys
	case *maasv1alpha1.Tenant:
		return t.Spec.APIKeys
	default:
		return nil
	}
}

func telemetryConfigFor(tenant client.Object) *maasv1alpha1.TenantTelemetryConfig {
	switch t := tenant.(type) {
	case *maasv1alpha1.MaasTenantConfig:
		return t.Spec.Telemetry
	case *maasv1alpha1.Tenant:
		return t.Spec.Telemetry
	default:
		return nil
	}
}

// applyPlatformParams patches all dynamic values into rendered resources.
func applyPlatformParams(log logr.Logger, resources []unstructured.Unstructured, params PlatformParams) error {
	for i := range resources {
		if err := patchResource(log, &resources[i], params); err != nil {
			return err
		}
	}
	return nil
}

// patchResource applies tenant-specific patches to a single resource.
func patchResource(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	gvk := r.GroupVersionKind()
	name := r.GetName()
	tenantID := params.TenantIdentifier

	switch {
	case gvk == GVKDeployment && name == baseMaaSAPIDeploymentName:
		// Rename and patch maas-api Deployment for this tenant
		r.SetName(MaaSAPIDeploymentName(tenantID))
		return patchMaaSAPIDeployment(log, r, params)
	case gvk == GVKDeployment && name == PayloadProcessingName:
		return patchPayloadProcessingDeployment(log, r, params)
	case gvk == GVKCronJob && name == baseMaaSAPIKeyCleanupCronJobName:
		// Rename and patch cleanup CronJob for this tenant
		r.SetName(MaaSAPIKeyCleanupCronJobName(tenantID))
		return patchCleanupCronJobImage(log, r, params)
	case gvk == GVKHTTPRoute && name == baseMaaSAPIRouteName:
		// Rename and patch HTTPRoute for this tenant
		r.SetName(MaaSAPIRouteName(tenantID))
		return patchHTTPRoute(log, r, params)
	case gvk == GVKDestinationRule && name == baseGatewayDestinationRuleName:
		// Rename and patch DestinationRule for this tenant
		r.SetName(GatewayDestinationRuleName(tenantID))
		return patchMaaSAPIDestinationRule(log, r, params)
	case gvk == GVKDestinationRule && (name == PayloadProcessingName || name == PayloadPreProcessingName):
		return patchPayloadDestinationRule(log, r, params)
	case gvk == GVKEnvoyFilter && name == PayloadProcessingName:
		return patchPayloadProcessingEnvoyFilter(log, r, params)
	case gvk == GVKDeployment && name == PayloadPreProcessingName:
		return patchPreProcessingDeployment(r, params)
	case gvk == GVKService && name == baseMaaSAPIServiceName:
		// Rename and patch maas-api Service for this tenant
		r.SetName(MaaSAPIServiceName(tenantID))
		return patchMaaSAPIService(log, r, params)
	case gvk == GVKService && (name == PayloadProcessingName || name == PayloadPreProcessingName):
		r.SetNamespace(params.GatewayNamespace)
	case gvk == GVKServiceAccount && name == PayloadProcessingName:
		r.SetNamespace(params.GatewayNamespace)
	case gvk == GVKConfigMap && name == PayloadProcessingPluginsConfigMapName:
		r.SetNamespace(params.GatewayNamespace)
	case gvk == GVKClusterRoleBinding && name == PayloadProcessingReaderClusterRoleBindingName:
		return patchClusterRoleBindingSubjectNS(r, params.GatewayNamespace)
	}
	return nil
}

func patchMaaSAPIDeployment(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	log.V(4).Info("Patching maas-api image", "image", params.MaaSAPIImage)
	if err := setContainerImage(r, "maas-api", params.MaaSAPIImage); err != nil {
		return fmt.Errorf("patch maas-api image: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "GATEWAY_NAMESPACE", params.GatewayNamespace); err != nil {
		return fmt.Errorf("patch GATEWAY_NAMESPACE: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "GATEWAY_NAME", params.GatewayName); err != nil {
		return fmt.Errorf("patch GATEWAY_NAME: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "MAAS_SUBSCRIPTION_NAMESPACE", params.SubscriptionNamespace); err != nil {
		return fmt.Errorf("patch MAAS_SUBSCRIPTION_NAMESPACE: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "API_KEY_MAX_EXPIRATION_DAYS", params.APIKeyMaxExpirationDays); err != nil {
		return fmt.Errorf("patch API_KEY_MAX_EXPIRATION_DAYS: %w", err)
	}

	// Set TENANT_NAME environment variable for per-tenant maas-api instances.
	// This value is used by maas-api for database queries (WHERE tenant = $TENANT_NAME)
	// and for validating the X-MaaS-Tenant header from Authorino.
	// Value: "models-as-a-service" for default tenant, tenant name (e.g., "redteam") for AITenant-managed tenants.
	// Note: TenantIdentifier is "" for default tenant (used for resource naming),
	// but TENANT_NAME must be "models-as-a-service" for DB consistency.
	tenantName := params.TenantIdentifier
	if tenantName == "" {
		// Default tenant: resource names use empty string (e.g., "maas-api"),
		// but TENANT_NAME must match DB default and AuthPolicy header value
		tenantName = "models-as-a-service"
	}
	if err := setOrAddEnvVar(r, "maas-api", "TENANT_NAME", tenantName); err != nil {
		return fmt.Errorf("patch TENANT_NAME: %w", err)
	}

	// Add tenant-instance label to pod template for unique Service selector matching.
	// This ensures each tenant's Service only routes to its own pods.
	// Use deployment name as the label value since it's already unique per tenant.
	deploymentName := MaaSAPIDeploymentName(params.TenantIdentifier)
	if err := addPodTemplateLabel(r, "maas.opendatahub.io/tenant-instance", deploymentName); err != nil {
		return fmt.Errorf("patch tenant-instance label: %w", err)
	}

	return nil
}

func patchMaaSAPIService(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	// Add tenant-instance label to Service selector to ensure it only routes to its own pods.
	// This matches the label we added to the Deployment's pod template.
	deploymentName := MaaSAPIDeploymentName(params.TenantIdentifier)
	if err := addServiceSelectorLabel(r, "maas.opendatahub.io/tenant-instance", deploymentName); err != nil {
		return fmt.Errorf("patch tenant-instance selector: %w", err)
	}
	return nil
}

func patchPayloadProcessingDeployment(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	r.SetNamespace(params.GatewayNamespace)
	log.V(4).Info("Patching payload-processing image", "image", params.PayloadProcessingImage)
	if err := setContainerImage(r, "payload-processing", params.PayloadProcessingImage); err != nil {
		return fmt.Errorf("patch payload-processing image: %w", err)
	}
	return nil
}

func patchPreProcessingDeployment(r *unstructured.Unstructured, params PlatformParams) error {
	r.SetNamespace(params.GatewayNamespace)
	if params.PayloadProcessingImage != "" {
		if err := setContainerImage(r, PayloadPreProcessingName, params.PayloadProcessingImage); err != nil {
			return fmt.Errorf("patch payload-pre-processing image: %w", err)
		}
	}
	return nil
}

func patchCleanupCronJobImage(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	log.V(4).Info("Patching cleanup CronJob image", "image", params.MaaSAPIKeyCleanupImage)
	if err := setCronJobContainerImage(r, "cleanup", params.MaaSAPIKeyCleanupImage); err != nil {
		return fmt.Errorf("patch cleanup CronJob image: %w", err)
	}

	// Patch the cleanup command to use tenant-specific service name
	containers, found, err := unstructured.NestedSlice(r.Object,
		"spec", "jobTemplate", "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("read cleanup CronJob containers: %w", err)
	}
	if found && len(containers) > 0 {
		container, ok := containers[0].(map[string]any)
		if !ok {
			return errors.New("cleanup CronJob container is not a map")
		}
		command, ok := container["command"].([]any)
		if ok && len(command) > 0 {
			tenantServiceName := MaaSAPIServiceName(params.TenantIdentifier)
			// Look for the curl command with maas-api:8443
			modified := false
			for i, cmdInterface := range command {
				if cmd, ok := cmdInterface.(string); ok && strings.Contains(cmd, "maas-api:8443") {
					// Replace maas-api with tenant-specific service name
					newCmd := strings.ReplaceAll(cmd, "maas-api:8443", tenantServiceName+":8443")
					command[i] = newCmd
					modified = true
					log.V(4).Info("Patching cleanup CronJob command URL", "old", "maas-api:8443", "new", tenantServiceName+":8443")
				}
			}
			if modified {
				container["command"] = command
				containers[0] = container
				if err := unstructured.SetNestedSlice(r.Object, containers,
					"spec", "jobTemplate", "spec", "template", "spec", "containers"); err != nil {
					return fmt.Errorf("write cleanup CronJob containers: %w", err)
				}
			}
		}
	}

	return nil
}

func patchHTTPRoute(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	log.V(4).Info("Patching HTTPRoute parentRefs", "namespace", params.GatewayNamespace, "name", params.GatewayName)
	parentRefs, found, err := unstructured.NestedSlice(r.Object, "spec", "parentRefs")
	if err != nil {
		return fmt.Errorf("read HTTPRoute parentRefs: %w", err)
	}
	if !found || len(parentRefs) == 0 {
		return errors.New("HTTPRoute parentRefs not found")
	}
	ref, ok := parentRefs[0].(map[string]any)
	if !ok {
		return errors.New("HTTPRoute parentRefs[0] is not an object")
	}
	ref["namespace"] = params.GatewayNamespace
	ref["name"] = params.GatewayName
	parentRefs[0] = ref
	if err := unstructured.SetNestedSlice(r.Object, parentRefs, "spec", "parentRefs"); err != nil {
		return fmt.Errorf("write HTTPRoute parentRefs: %w", err)
	}

	// Patch backendRefs to point to the per-tenant maas-api Service.
	// The HTTPRoute has multiple rules (for /v1/models and /maas-api paths),
	// and each rule has backendRefs that need to be updated.
	tenantServiceName := MaaSAPIServiceName(params.TenantIdentifier)
	rules, found, err := unstructured.NestedSlice(r.Object, "spec", "rules")
	if err != nil {
		return fmt.Errorf("read HTTPRoute rules: %w", err)
	}
	if !found {
		return errors.New("HTTPRoute rules not found")
	}

	for i, ruleRaw := range rules {
		rule, ok := ruleRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("HTTPRoute rule[%d] is not an object", i)
		}
		backendRefs, found, err := unstructured.NestedSlice(rule, "backendRefs")
		if err != nil {
			return fmt.Errorf("read HTTPRoute rule[%d] backendRefs: %w", i, err)
		}
		if !found {
			return fmt.Errorf("HTTPRoute rule[%d] has no backendRefs", i)
		}
		rewritten := false
		for j, backendRefRaw := range backendRefs {
			backendRef, ok := backendRefRaw.(map[string]any)
			if !ok {
				return fmt.Errorf("HTTPRoute rule[%d] backendRef[%d] is not an object", i, j)
			}
			// Update the Service name to the per-tenant Service
			if name, exists := backendRef["name"]; exists && name == "maas-api" {
				backendRef["name"] = tenantServiceName
				backendRefs[j] = backendRef
				rewritten = true
			}
		}
		if !rewritten {
			return fmt.Errorf("HTTPRoute rule[%d] has no \"maas-api\" backendRef to rewrite", i)
		}
		if err := unstructured.SetNestedSlice(rule, backendRefs, "backendRefs"); err != nil {
			return fmt.Errorf("write HTTPRoute rule[%d] backendRefs: %w", i, err)
		}
		rules[i] = rule
	}

	if err := unstructured.SetNestedSlice(r.Object, rules, "spec", "rules"); err != nil {
		return fmt.Errorf("write HTTPRoute rules: %w", err)
	}

	log.V(4).Info("Patched HTTPRoute backendRefs", "service", tenantServiceName)
	return nil
}

func patchMaaSAPIDestinationRule(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	r.SetNamespace(params.GatewayNamespace)
	host, found, err := unstructured.NestedString(r.Object, "spec", "host")
	if err != nil {
		return fmt.Errorf("read maas-api DestinationRule host: %w", err)
	}
	if !found {
		return errors.New("maas-api DestinationRule host not found")
	}
	if host != "" {
		newHost := fmt.Sprintf("%s.%s.svc.cluster.local", MaaSAPIServiceName(params.TenantIdentifier), params.AppNamespace)
		log.V(4).Info("Patching maas-api DestinationRule host", "old", host, "new", newHost)
		if err := unstructured.SetNestedField(r.Object, newHost, "spec", "host"); err != nil {
			return fmt.Errorf("write maas-api DestinationRule host: %w", err)
		}
	}
	return nil
}

func patchPayloadDestinationRule(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	name := r.GetName()
	r.SetNamespace(params.GatewayNamespace)
	host, found, err := unstructured.NestedString(r.Object, "spec", "host")
	if err != nil {
		return fmt.Errorf("read %s DestinationRule host: %w", name, err)
	}
	if found && host != "" {
		newHost := replaceHostNamespace(host, params.GatewayNamespace)
		log.V(4).Info("Patching payload DestinationRule host", "name", name, "old", host, "new", newHost)
		if err := unstructured.SetNestedField(r.Object, newHost, "spec", "host"); err != nil {
			return fmt.Errorf("write %s DestinationRule host: %w", name, err)
		}
	}
	return nil
}

const rhclWasmFilterName = "envoy.filters.http.wasm"

func wasmpluginAnchorName(gatewayNamespace, gatewayName string) string {
	return fmt.Sprintf("extensions.istio.io/wasmplugin/%s.kuadrant-%s", gatewayNamespace, gatewayName)
}

func grpcClusterName(service, namespace string, port int) string {
	return fmt.Sprintf("outbound|%d||%s.%s.svc.cluster.local", port, service, namespace)
}

func patchPayloadProcessingEnvoyFilter(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	r.SetNamespace(params.GatewayNamespace)

	targetRefs, found, err := unstructured.NestedSlice(r.Object, "spec", "targetRefs")
	if err != nil {
		return fmt.Errorf("read EnvoyFilter targetRefs: %w", err)
	}
	if !found || len(targetRefs) == 0 {
		return errors.New("EnvoyFilter targetRefs not found")
	}
	ref, ok := targetRefs[0].(map[string]any)
	if !ok {
		return errors.New("EnvoyFilter targetRefs[0] is not an object")
	}
	ref["name"] = params.GatewayName
	targetRefs[0] = ref
	if err := unstructured.SetNestedSlice(r.Object, targetRefs, "spec", "targetRefs"); err != nil {
		return fmt.Errorf("write EnvoyFilter targetRefs: %w", err)
	}

	anchorName := wasmpluginAnchorName(params.GatewayNamespace, params.GatewayName)
	beforeCluster := grpcClusterName(PayloadPreProcessingName, params.GatewayNamespace, 9004)
	afterCluster := grpcClusterName(PayloadProcessingName, params.GatewayNamespace, 9004)

	configPatches, found, err := unstructured.NestedSlice(r.Object, "spec", "configPatches")
	if err != nil {
		return fmt.Errorf("read EnvoyFilter configPatches: %w", err)
	}
	const (
		filterPatchCount      = 4 // WasmPlugin pair + RHCL 1.4 wasm pair
		routeDisablePatchBase = filterPatchCount
		totalConfigPatches    = routeDisablePatchBase + 4
	)
	if !found || len(configPatches) < totalConfigPatches {
		return fmt.Errorf("EnvoyFilter configPatches: expected at least %d entries, got %d", totalConfigPatches, len(configPatches))
	}

	clusterByIndex := []string{beforeCluster, afterCluster, beforeCluster, afterCluster}
	subFilterByIndex := []string{anchorName, anchorName, rhclWasmFilterName, rhclWasmFilterName}

	for i := 0; i < filterPatchCount; i++ {
		patch, ok := configPatches[i].(map[string]any)
		if !ok {
			return fmt.Errorf("EnvoyFilter configPatches[%d] is not an object", i)
		}

		subFilterPath := []string{"match", "listener", "filterChain", "filter", "subFilter", "name"}
		if err := unstructured.SetNestedField(patch, subFilterByIndex[i], subFilterPath...); err != nil {
			return fmt.Errorf("write configPatches[%d] subFilter.name: %w", i, err)
		}

		clusterPath := []string{"patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name"}
		if err := unstructured.SetNestedField(patch, clusterByIndex[i], clusterPath...); err != nil {
			return fmt.Errorf("write configPatches[%d] grpc cluster_name: %w", i, err)
		}

		configPatches[i] = patch
	}

	// Patches 4–7 disable ext_proc on all non-inference maas-api routes.
	// Route name uses Istio's Gateway API convention: <namespace>.<httproute-name>.<rule-index>.
	// Rule indices: 0=/v1/models, 1=/v1/subscriptions, 2=/v1/api-keys, 3=/maas-api/*
	for i := routeDisablePatchBase; i < totalConfigPatches; i++ {
		patch, ok := configPatches[i].(map[string]any)
		if !ok {
			return fmt.Errorf("EnvoyFilter configPatches[%d] is not an object", i)
		}
		if err := unstructured.SetNestedField(patch,
			fmt.Sprintf("%s.%s.%d", params.AppNamespace, MaaSAPIRouteName(params.TenantIdentifier), i-routeDisablePatchBase),
			"match", "routeConfiguration", "vhost", "route", "name"); err != nil {
			return fmt.Errorf("write configPatches[%d] route name: %w", i, err)
		}
	}

	if err := unstructured.SetNestedSlice(r.Object, configPatches, "spec", "configPatches"); err != nil {
		return fmt.Errorf("write EnvoyFilter configPatches: %w", err)
	}
	return nil
}

func patchClusterRoleBindingSubjectNS(r *unstructured.Unstructured, ns string) error {
	subjects, found, err := unstructured.NestedSlice(r.Object, "subjects")
	if err != nil {
		return fmt.Errorf("read ClusterRoleBinding subjects: %w", err)
	}
	if !found || len(subjects) == 0 {
		return errors.New("ClusterRoleBinding subjects not found")
	}
	subj, ok := subjects[0].(map[string]any)
	if !ok {
		return errors.New("ClusterRoleBinding subjects[0] is not an object")
	}
	subj["namespace"] = ns
	subjects[0] = subj
	if err := unstructured.SetNestedSlice(r.Object, subjects, "subjects"); err != nil {
		return fmt.Errorf("write ClusterRoleBinding subjects: %w", err)
	}
	return nil
}

// replaceHostNamespace replaces the second segment of a dot-separated FQDN.
// e.g. "maas-api.maas-api.svc.cluster.local" → "maas-api.opendatahub.svc.cluster.local"
func replaceHostNamespace(host, ns string) string {
	parts := strings.SplitN(host, ".", 3)
	if len(parts) >= 2 {
		parts[1] = ns
		return strings.Join(parts, ".")
	}
	return host
}

func setContainerImage(r *unstructured.Unstructured, containerName, image string) error {
	containers, found, err := unstructured.NestedSlice(r.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return errors.New("containers not found")
	}
	for i, c := range containers {
		if cm, ok := c.(map[string]any); ok && cm["name"] == containerName {
			cm["image"] = image
			containers[i] = cm
			return unstructured.SetNestedSlice(r.Object, containers, "spec", "template", "spec", "containers")
		}
	}
	return fmt.Errorf("container %q not found", containerName)
}

func setOrAddEnvVar(r *unstructured.Unstructured, containerName, envName, envValue string) error {
	containers, found, err := unstructured.NestedSlice(r.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return errors.New("containers not found")
	}
	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != containerName {
			continue
		}
		envSlice, _ := cm["env"].([]any)
		for j, e := range envSlice {
			if em, ok := e.(map[string]any); ok && em["name"] == envName {
				em["value"] = envValue
				delete(em, "valueFrom")
				envSlice[j] = em
				cm["env"] = envSlice
				containers[i] = cm
				return unstructured.SetNestedSlice(r.Object, containers, "spec", "template", "spec", "containers")
			}
		}
		envSlice = append(envSlice, map[string]any{"name": envName, "value": envValue})
		cm["env"] = envSlice
		containers[i] = cm
		return unstructured.SetNestedSlice(r.Object, containers, "spec", "template", "spec", "containers")
	}
	return fmt.Errorf("container %q not found", containerName)
}

func setCronJobContainerImage(r *unstructured.Unstructured, containerName, image string) error {
	containers, found, err := unstructured.NestedSlice(r.Object, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	if err != nil || !found {
		return errors.New("containers not found")
	}
	for i, c := range containers {
		if cm, ok := c.(map[string]any); ok && cm["name"] == containerName {
			cm["image"] = image
			containers[i] = cm
			return unstructured.SetNestedSlice(r.Object, containers, "spec", "jobTemplate", "spec", "template", "spec", "containers")
		}
	}
	return fmt.Errorf("container %q not found", containerName)
}

// addPodTemplateLabel adds a label to the Deployment's pod template spec.
// This label will be set on all pods created by the Deployment.
func addPodTemplateLabel(r *unstructured.Unstructured, key, value string) error {
	labels, found, err := unstructured.NestedStringMap(r.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return fmt.Errorf("read pod template labels: %w", err)
	}
	if !found || labels == nil {
		labels = make(map[string]string)
	}
	labels[key] = value
	return unstructured.SetNestedStringMap(r.Object, labels, "spec", "template", "metadata", "labels")
}

// addServiceSelectorLabel adds a label to the Service selector.
// This ensures the Service only routes to pods with matching labels.
func addServiceSelectorLabel(r *unstructured.Unstructured, key, value string) error {
	selector, found, err := unstructured.NestedStringMap(r.Object, "spec", "selector")
	if err != nil {
		return fmt.Errorf("read service selector: %w", err)
	}
	if !found || selector == nil {
		selector = make(map[string]string)
	}
	selector[key] = value
	return unstructured.SetNestedStringMap(r.Object, selector, "spec", "selector")
}
