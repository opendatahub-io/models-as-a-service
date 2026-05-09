package tenantreconcile

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// PlatformParams holds resolved runtime values for PostRender patching.
type PlatformParams struct {
	AppNamespace     string
	GatewayNamespace string
	GatewayName      string
	ClusterAudience  string

	MaaSAPIImage             string
	PayloadProcessingImage   string
	MaaSAPIKeyCleanupImage   string

	APIKeyMaxExpirationDays string
}

// BuildPlatformParams resolves all runtime parameters from the Tenant CR,
// cluster state, and RELATED_IMAGE_* env vars. No disk I/O.
func BuildPlatformParams(tenant *maasv1alpha1.Tenant, appNamespace, clusterAudience string) PlatformParams {
	p := PlatformParams{
		AppNamespace:     appNamespace,
		GatewayNamespace: tenant.Spec.GatewayRef.Namespace,
		GatewayName:      tenant.Spec.GatewayRef.Name,
		ClusterAudience:  clusterAudience,
	}

	if v := os.Getenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE"); v != "" {
		p.MaaSAPIImage = v
	}
	if v := os.Getenv("RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE"); v != "" {
		p.PayloadProcessingImage = v
	}
	if v := os.Getenv("RELATED_IMAGE_UBI_MINIMAL_IMAGE"); v != "" {
		p.MaaSAPIKeyCleanupImage = v
	}

	p.APIKeyMaxExpirationDays = "90"
	if tenant.Spec.APIKeys != nil && tenant.Spec.APIKeys.MaxExpirationDays != nil {
		p.APIKeyMaxExpirationDays = strconv.FormatInt(int64(*tenant.Spec.APIKeys.MaxExpirationDays), 10)
	}

	return p
}

// applyPlatformParams patches all dynamic values into rendered resources.
func applyPlatformParams(log logr.Logger, resources []unstructured.Unstructured, params PlatformParams) error {
	for i := range resources {
		r := &resources[i]
		gvk := r.GroupVersionKind()
		name := r.GetName()

		switch {
		case gvk == GVKDeployment && name == MaaSAPIDeploymentName:
			if err := patchMaaSAPIDeployment(log, r, params); err != nil {
				return err
			}
		case gvk == GVKDeployment && name == "payload-processing":
			patchPayloadProcessingDeployment(log, r, params)
		case gvk == GVKCronJob && name == "maas-api-key-cleanup":
			patchCleanupCronJobImage(log, r, params)
		case gvk == GVKHTTPRoute && name == "maas-api-route":
			patchHTTPRoute(log, r, params)
		case gvk == GVKAuthPolicy && name == MaaSAPIAuthPolicyName:
			if err := patchMaaSAPIAuthPolicy(log, r, params); err != nil {
				return err
			}
		case gvk == GVKDestinationRule && name == "maas-api-backend-tls":
			patchMaaSAPIDestinationRule(log, r, params)
		case gvk == GVKDestinationRule && name == "payload-processing":
			patchPayloadProcessingDestinationRule(log, r, params)
		case gvk == GVKEnvoyFilter && name == "payload-processing":
			patchPayloadProcessingEnvoyFilter(log, r, params)
		case gvk.Kind == "Service" && name == "payload-processing":
			r.SetNamespace(params.GatewayNamespace)
		case gvk.Kind == "ServiceAccount" && name == "payload-processing":
			r.SetNamespace(params.GatewayNamespace)
		case gvk.Kind == "ConfigMap" && name == "payload-processing-plugins":
			r.SetNamespace(params.GatewayNamespace)
		case gvk.Kind == "ClusterRoleBinding" && name == "payload-processing-reader":
			patchClusterRoleBindingSubjectNS(r, params.GatewayNamespace)
		}
	}
	return nil
}

func patchMaaSAPIDeployment(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	if params.MaaSAPIImage != "" {
		log.V(4).Info("Patching maas-api image", "image", params.MaaSAPIImage)
		if err := setContainerImage(r, "maas-api", params.MaaSAPIImage); err != nil {
			return fmt.Errorf("patch maas-api image: %w", err)
		}
	}
	if err := setOrAddEnvVar(r, "maas-api", "GATEWAY_NAMESPACE", params.GatewayNamespace); err != nil {
		return fmt.Errorf("patch GATEWAY_NAMESPACE: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "GATEWAY_NAME", params.GatewayName); err != nil {
		return fmt.Errorf("patch GATEWAY_NAME: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "API_KEY_MAX_EXPIRATION_DAYS", params.APIKeyMaxExpirationDays); err != nil {
		return fmt.Errorf("patch API_KEY_MAX_EXPIRATION_DAYS: %w", err)
	}
	return nil
}

func patchPayloadProcessingDeployment(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	r.SetNamespace(params.GatewayNamespace)
	if params.PayloadProcessingImage != "" {
		log.V(4).Info("Patching payload-processing image", "image", params.PayloadProcessingImage)
		_ = setContainerImage(r, "payload-processing", params.PayloadProcessingImage)
	}
}

func patchCleanupCronJobImage(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	if params.MaaSAPIKeyCleanupImage != "" {
		log.V(4).Info("Patching cleanup CronJob image", "image", params.MaaSAPIKeyCleanupImage)
		_ = unstructured.SetNestedField(r.Object, params.MaaSAPIKeyCleanupImage,
			"spec", "jobTemplate", "spec", "template", "spec", "containers", "0", "image")
	}
}

func patchHTTPRoute(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	log.V(4).Info("Patching HTTPRoute parentRefs", "namespace", params.GatewayNamespace, "name", params.GatewayName)
	parentRefs, _, _ := unstructured.NestedSlice(r.Object, "spec", "parentRefs")
	if len(parentRefs) > 0 {
		if ref, ok := parentRefs[0].(map[string]any); ok {
			ref["namespace"] = params.GatewayNamespace
			ref["name"] = params.GatewayName
			_ = unstructured.SetNestedSlice(r.Object, parentRefs, "spec", "parentRefs")
		}
	}
}

func patchMaaSAPIAuthPolicy(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	if params.ClusterAudience != "" {
		log.V(4).Info("Patching AuthPolicy cluster-audience", "audience", params.ClusterAudience)
		audiences, _, _ := unstructured.NestedSlice(r.Object,
			"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences")
		if len(audiences) > 0 {
			audiences[0] = params.ClusterAudience
			_ = unstructured.SetNestedSlice(r.Object, audiences,
				"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences")
		}
	}

	url, _, _ := unstructured.NestedString(r.Object,
		"spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	if url != "" && strings.Contains(url, ".placehold.") {
		newURL := strings.Replace(url, ".placehold.", "."+params.AppNamespace+".", 1)
		log.V(4).Info("Patching AuthPolicy validation URL", "old", url, "new", newURL)
		_ = unstructured.SetNestedField(r.Object, newURL,
			"spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	}
	return nil
}

func patchMaaSAPIDestinationRule(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	r.SetNamespace(params.GatewayNamespace)
	host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
	if host != "" {
		newHost := replaceHostNamespace(host, params.AppNamespace)
		log.V(4).Info("Patching maas-api DestinationRule host", "old", host, "new", newHost)
		_ = unstructured.SetNestedField(r.Object, newHost, "spec", "host")
	}
}

func patchPayloadProcessingDestinationRule(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	r.SetNamespace(params.GatewayNamespace)
	host, _, _ := unstructured.NestedString(r.Object, "spec", "host")
	if host != "" {
		newHost := replaceHostNamespace(host, params.GatewayNamespace)
		log.V(4).Info("Patching payload-processing DestinationRule host", "old", host, "new", newHost)
		_ = unstructured.SetNestedField(r.Object, newHost, "spec", "host")
	}
}

func patchPayloadProcessingEnvoyFilter(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) {
	r.SetNamespace(params.GatewayNamespace)

	targetRefs, _, _ := unstructured.NestedSlice(r.Object, "spec", "targetRefs")
	if len(targetRefs) > 0 {
		if ref, ok := targetRefs[0].(map[string]any); ok {
			ref["name"] = params.GatewayName
			_ = unstructured.SetNestedSlice(r.Object, targetRefs, "spec", "targetRefs")
		}
	}

	clusterName, _, _ := unstructured.NestedString(r.Object,
		"spec", "configPatches", "0", "patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name")
	if clusterName != "" {
		newCluster := replaceHostNamespace(clusterName, params.GatewayNamespace)
		log.V(4).Info("Patching EnvoyFilter cluster_name", "old", clusterName, "new", newCluster)
		_ = unstructured.SetNestedField(r.Object, newCluster,
			"spec", "configPatches", "0", "patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name")
	}
}

func patchClusterRoleBindingSubjectNS(r *unstructured.Unstructured, ns string) {
	subjects, _, _ := unstructured.NestedSlice(r.Object, "subjects")
	if len(subjects) > 0 {
		if subj, ok := subjects[0].(map[string]any); ok {
			subj["namespace"] = ns
			_ = unstructured.SetNestedSlice(r.Object, subjects, "subjects")
		}
	}
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
		return fmt.Errorf("containers not found")
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
		return fmt.Errorf("containers not found")
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

