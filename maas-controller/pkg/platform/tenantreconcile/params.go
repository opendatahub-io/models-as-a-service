package tenantreconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	TenantNamespace  string
	TenantName       string
	DedicatedMaaSAPI bool
	GatewayNamespace string
	GatewayName      string
	ClusterAudience  string

	MaaSAPIName                      string
	MaaSAPIRouteName                 string
	MaaSAPIAuthPolicyName            string
	MaaSAPIKeyCleanupCronJobName     string
	MaaSAPIServiceCertSecretName     string
	MaaSAPIAllowMonitoringPolicyName string
	MaaSAPIAllowAuthorinoPolicyName  string
	MaaSAPICleanupPolicyName         string
	MaaSAPIPodMonitorName            string
	MaaSAPISupplementalName          string

	MaaSAPIImage           string
	PayloadProcessingImage string
	MaaSAPIKeyCleanupImage string

	APIKeyMaxExpirationDays string
}

// BuildPlatformParams resolves all runtime parameters from the Tenant CR,
// cluster state, and RELATED_IMAGE_* env vars. No disk I/O.
func BuildPlatformParams(tenant *maasv1alpha1.Tenant, appNamespace, clusterAudience string) PlatformParams {
	tenantName := tenantNameForParams(tenant)
	dedicatedMaaSAPI := tenant.Namespace != "" && tenant.Namespace != "models-as-a-service" && tenantName != ""
	return PlatformParams{
		AppNamespace:                     appNamespace,
		TenantNamespace:                  tenant.Namespace,
		TenantName:                       tenantName,
		DedicatedMaaSAPI:                 dedicatedMaaSAPI,
		GatewayNamespace:                 tenant.Spec.GatewayRef.Namespace,
		GatewayName:                      tenant.Spec.GatewayRef.Name,
		ClusterAudience:                  clusterAudience,
		MaaSAPIName:                      tenantScopedName(MaaSAPIDeploymentName, tenantName, dedicatedMaaSAPI),
		MaaSAPIRouteName:                 tenantScopedName(MaaSAPIRouteName, tenantName, dedicatedMaaSAPI),
		MaaSAPIAuthPolicyName:            tenantScopedName(MaaSAPIAuthPolicyName, tenantName, dedicatedMaaSAPI),
		MaaSAPIKeyCleanupCronJobName:     tenantScopedName(MaaSAPIKeyCleanupCronJobName, tenantName, dedicatedMaaSAPI),
		MaaSAPIServiceCertSecretName:     tenantScopedName(MaaSAPIServiceCertSecretName, tenantName, dedicatedMaaSAPI),
		MaaSAPIAllowMonitoringPolicyName: tenantScopedName(MaaSAPIAllowMonitoringNetworkPolicyName, tenantName, dedicatedMaaSAPI),
		MaaSAPIAllowAuthorinoPolicyName:  tenantScopedName(MaaSAPIAllowAuthorinoNetworkPolicyName, tenantName, dedicatedMaaSAPI),
		MaaSAPICleanupPolicyName:         tenantScopedName(MaaSAPICleanupNetworkPolicyName, tenantName, dedicatedMaaSAPI),
		MaaSAPIPodMonitorName:            tenantScopedName(MaaSAPIPodMonitorName, tenantName, dedicatedMaaSAPI),
		MaaSAPISupplementalName:          tenantScopedName(MaaSAPISupplementalName, tenantName, dedicatedMaaSAPI),
		MaaSAPIImage:                     firstNonEmpty(os.Getenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE"), DefaultMaaSAPIImage),
		PayloadProcessingImage:           firstNonEmpty(os.Getenv("RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE"), DefaultPayloadProcessingImage),
		MaaSAPIKeyCleanupImage:           firstNonEmpty(os.Getenv("RELATED_IMAGE_UBI_MINIMAL_IMAGE"), DefaultMaaSAPIKeyCleanupImage),
		APIKeyMaxExpirationDays:          resolveAPIKeyMaxExpirationDays(tenant),
	}
}

func tenantNameForParams(tenant *maasv1alpha1.Tenant) string {
	if tenant.Labels != nil {
		if name := strings.TrimSpace(tenant.Labels[LabelAIGatewayTenant]); name != "" {
			return name
		}
		if name := strings.TrimSpace(tenant.Labels[LabelTenantName]); name != "" {
			return name
		}
	}
	if tenant.Namespace != "" {
		return tenant.Namespace
	}
	return tenant.Name
}

func tenantScopedName(base, tenantName string, dedicated bool) string {
	if !dedicated {
		return base
	}
	tenantName = dns1123LabelFragment(tenantName)
	if tenantName == "" {
		tenantName = "tenant"
	}
	raw := base + "-" + tenantName
	if len(raw) <= 63 {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])[:8]
	budget := 63 - len(hash) - 1
	prefix := strings.Trim(raw[:budget], "-")
	if prefix == "" {
		prefix = base
	}
	return prefix + "-" + hash
}

func dns1123LabelFragment(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		valid := r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')
		if !valid {
			r = '-'
		}
		if r == '-' {
			if b.Len() == 0 || lastDash {
				continue
			}
			lastDash = true
			b.WriteByte('-')
			continue
		}
		lastDash = false
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func resolveAPIKeyMaxExpirationDays(tenant *maasv1alpha1.Tenant) string {
	if tenant.Spec.APIKeys != nil && tenant.Spec.APIKeys.MaxExpirationDays != nil {
		return strconv.FormatInt(int64(*tenant.Spec.APIKeys.MaxExpirationDays), 10)
	}
	return DefaultAPIKeyMaxExpirationDays
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
		case gvk == GVKDeployment && name == PayloadProcessingName:
			if err := patchPayloadProcessingDeployment(log, r, params); err != nil {
				return err
			}
		case gvk == GVKCronJob && name == MaaSAPIKeyCleanupCronJobName:
			if err := patchCleanupCronJobImage(log, r, params); err != nil {
				return err
			}
		case gvk == GVKHTTPRoute && name == MaaSAPIRouteName:
			if err := patchHTTPRoute(log, r, params); err != nil {
				return err
			}
		case gvk == GVKAuthPolicy && name == MaaSAPIAuthPolicyName:
			if err := patchMaaSAPIAuthPolicy(log, r, params); err != nil {
				return err
			}
		case gvk == GVKDestinationRule && name == GatewayDestinationRuleName:
			if err := patchMaaSAPIDestinationRule(log, r, params); err != nil {
				return err
			}
		case gvk == GVKDestinationRule && (name == PayloadProcessingName || name == PayloadPreProcessingName):
			if err := patchPayloadDestinationRule(log, r, params); err != nil {
				return err
			}
		case gvk == GVKEnvoyFilter && name == PayloadProcessingName:
			if err := patchPayloadProcessingEnvoyFilter(log, r, params); err != nil {
				return err
			}
		case gvk == GVKDeployment && name == PayloadPreProcessingName:
			if err := patchPreProcessingDeployment(r, params); err != nil {
				return err
			}
		case gvk == GVKService && (name == PayloadProcessingName || name == PayloadPreProcessingName):
			r.SetNamespace(params.GatewayNamespace)
		case gvk == GVKServiceAccount && name == PayloadProcessingName:
			r.SetNamespace(params.GatewayNamespace)
		case gvk == GVKConfigMap && name == PayloadProcessingPluginsConfigMapName:
			r.SetNamespace(params.GatewayNamespace)
		case gvk == GVKClusterRoleBinding && name == PayloadProcessingReaderClusterRoleBindingName:
			if err := patchClusterRoleBindingSubjectNS(r, params.GatewayNamespace); err != nil {
				return err
			}
		}
		if err := scopeMaaSAPIResource(r, params); err != nil {
			return err
		}
	}
	return nil
}

func scopeMaaSAPIResource(r *unstructured.Unstructured, params PlatformParams) error {
	if !params.DedicatedMaaSAPI {
		return nil
	}
	gvk := r.GroupVersionKind()
	name := r.GetName()

	switch {
	case gvk == GVKDeployment && name == MaaSAPIDeploymentName:
		r.SetName(params.MaaSAPIName)
		if err := addTenantSelectorLabels(r, params, "spec", "selector", "matchLabels"); err != nil {
			return fmt.Errorf("scope maas-api deployment selector: %w", err)
		}
		if err := addTenantSelectorLabels(r, params, "spec", "template", "metadata", "labels"); err != nil {
			return fmt.Errorf("scope maas-api deployment pod labels: %w", err)
		}
		if err := unstructured.SetNestedField(r.Object, params.MaaSAPIName, "spec", "template", "spec", "serviceAccountName"); err != nil {
			return fmt.Errorf("scope maas-api deployment service account: %w", err)
		}
		if err := patchDeploymentSecretVolume(r, "maas-api-tls", params.MaaSAPIServiceCertSecretName); err != nil {
			return fmt.Errorf("scope maas-api serving cert: %w", err)
		}
	case gvk == GVKService && name == MaaSAPIDeploymentName:
		r.SetName(params.MaaSAPIName)
		if err := addTenantSelectorLabels(r, params, "spec", "selector"); err != nil {
			return fmt.Errorf("scope maas-api service selector: %w", err)
		}
		annotations := r.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["service.beta.openshift.io/serving-cert-secret-name"] = params.MaaSAPIServiceCertSecretName
		r.SetAnnotations(annotations)
	case gvk == GVKServiceAccount && name == MaaSAPIDeploymentName:
		r.SetName(params.MaaSAPIName)
	case gvk == GVKHTTPRoute && name == MaaSAPIRouteName:
		r.SetName(params.MaaSAPIRouteName)
		if err := patchHTTPRouteBackendRefs(r, params.MaaSAPIName); err != nil {
			return err
		}
	case gvk == GVKAuthPolicy && name == MaaSAPIAuthPolicyName:
		r.SetName(params.MaaSAPIAuthPolicyName)
		if err := unstructured.SetNestedField(r.Object, params.MaaSAPIRouteName, "spec", "targetRef", "name"); err != nil {
			return fmt.Errorf("scope maas-api AuthPolicy targetRef: %w", err)
		}
		if err := patchMaaSAPIAuthPolicyURL(r, params); err != nil {
			return err
		}
	case gvk == GVKDestinationRule && name == GatewayDestinationRuleName:
		r.SetName(tenantScopedName(GatewayDestinationRuleName, params.TenantName, true))
		host, found, err := unstructured.NestedString(r.Object, "spec", "host")
		if err != nil {
			return fmt.Errorf("read scoped maas-api DestinationRule host: %w", err)
		}
		if found && host != "" {
			if err := unstructured.SetNestedField(r.Object, replaceHostServiceAndNamespace(host, params.MaaSAPIName, params.AppNamespace), "spec", "host"); err != nil {
				return fmt.Errorf("write scoped maas-api DestinationRule host: %w", err)
			}
		}
	case gvk == GVKCronJob && name == MaaSAPIKeyCleanupCronJobName:
		r.SetName(params.MaaSAPIKeyCleanupCronJobName)
		if err := unstructured.SetNestedField(r.Object, params.MaaSAPIName, "spec", "jobTemplate", "spec", "template", "spec", "serviceAccountName"); err != nil {
			return fmt.Errorf("scope maas-api cleanup service account: %w", err)
		}
		if err := addTenantSelectorLabels(r, params, "spec", "jobTemplate", "spec", "template", "metadata", "labels"); err != nil {
			return fmt.Errorf("scope maas-api cleanup pod labels: %w", err)
		}
		if err := patchCleanupCronJobCommand(r, params.MaaSAPIName); err != nil {
			return err
		}
	case gvk == GVKNetworkPolicy && name == MaaSAPIAllowMonitoringNetworkPolicyName:
		r.SetName(params.MaaSAPIAllowMonitoringPolicyName)
		if err := addTenantSelectorLabels(r, params, "spec", "podSelector", "matchLabels"); err != nil {
			return fmt.Errorf("scope maas-api monitoring NetworkPolicy selector: %w", err)
		}
	case gvk == GVKNetworkPolicy && name == MaaSAPIAllowAuthorinoNetworkPolicyName:
		r.SetName(params.MaaSAPIAllowAuthorinoPolicyName)
		if err := addTenantSelectorLabels(r, params, "spec", "podSelector", "matchLabels"); err != nil {
			return fmt.Errorf("scope maas-api authorino NetworkPolicy selector: %w", err)
		}
	case gvk == GVKNetworkPolicy && name == MaaSAPICleanupNetworkPolicyName:
		r.SetName(params.MaaSAPICleanupPolicyName)
		if err := addTenantSelectorLabels(r, params, "spec", "podSelector", "matchLabels"); err != nil {
			return fmt.Errorf("scope maas-api cleanup NetworkPolicy selector: %w", err)
		}
		if err := patchCleanupNetworkPolicyEgress(r, params); err != nil {
			return err
		}
	case gvk == GVKPodMonitor && name == MaaSAPIPodMonitorName:
		r.SetName(params.MaaSAPIPodMonitorName)
		if err := addTenantSelectorLabels(r, params, "spec", "selector", "matchLabels"); err != nil {
			return fmt.Errorf("scope maas-api PodMonitor selector: %w", err)
		}
	case gvk == GVKClusterRoleBinding && name == MaaSAPIDeploymentName:
		r.SetName(params.MaaSAPIName)
		if err := patchClusterRoleBindingSubject(r, params.MaaSAPIName, params.AppNamespace); err != nil {
			return err
		}
	case gvk == GVKClusterRoleBinding && name == MaaSAPISupplementalName:
		r.SetName(params.MaaSAPISupplementalName)
		if err := patchClusterRoleBindingSubject(r, params.MaaSAPIName, params.AppNamespace); err != nil {
			return err
		}
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
	if err := setOrAddEnvVar(r, "maas-api", "API_KEY_MAX_EXPIRATION_DAYS", params.APIKeyMaxExpirationDays); err != nil {
		return fmt.Errorf("patch API_KEY_MAX_EXPIRATION_DAYS: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "MAAS_SUBSCRIPTION_NAMESPACE", params.TenantNamespace); err != nil {
		return fmt.Errorf("patch MAAS_SUBSCRIPTION_NAMESPACE: %w", err)
	}
	if err := setOrAddEnvVar(r, "maas-api", "TENANT_NAME", params.TenantName); err != nil {
		return fmt.Errorf("patch TENANT_NAME: %w", err)
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
	return nil
}

func patchMaaSAPIAuthPolicy(log logr.Logger, r *unstructured.Unstructured, params PlatformParams) error {
	log.V(4).Info("Patching AuthPolicy cluster-audience", "audience", params.ClusterAudience)
	audiences, found, err := unstructured.NestedSlice(r.Object,
		"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences")
	if err != nil {
		return fmt.Errorf("read AuthPolicy audiences: %w", err)
	}
	if !found || len(audiences) == 0 {
		return errors.New("AuthPolicy audiences not found")
	}
	audiences[0] = params.ClusterAudience
	if err := unstructured.SetNestedSlice(r.Object, audiences,
		"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences"); err != nil {
		return fmt.Errorf("write AuthPolicy audiences: %w", err)
	}

	url, found, err := unstructured.NestedString(r.Object,
		"spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	if err != nil {
		return fmt.Errorf("read AuthPolicy validation URL: %w", err)
	}
	if !found {
		return errors.New("AuthPolicy validation URL not found")
	}
	if url != "" && strings.Contains(url, ".placehold.") {
		newURL := strings.Replace(url, ".placehold.", "."+params.AppNamespace+".", 1)
		log.V(4).Info("Patching AuthPolicy validation URL", "old", url, "new", newURL)
		if err := unstructured.SetNestedField(r.Object, newURL,
			"spec", "rules", "metadata", "apiKeyValidation", "http", "url"); err != nil {
			return fmt.Errorf("write AuthPolicy validation URL: %w", err)
		}
	}
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
		newHost := replaceHostNamespace(host, params.AppNamespace)
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
	if !found || len(configPatches) < 2 {
		return fmt.Errorf("EnvoyFilter configPatches: expected at least 2 entries, got %d", len(configPatches))
	}

	clusterByIndex := []string{beforeCluster, afterCluster}

	for i, clusterName := range clusterByIndex {
		patch, ok := configPatches[i].(map[string]any)
		if !ok {
			return fmt.Errorf("EnvoyFilter configPatches[%d] is not an object", i)
		}

		subFilterPath := []string{"match", "listener", "filterChain", "filter", "subFilter", "name"}
		if err := unstructured.SetNestedField(patch, anchorName, subFilterPath...); err != nil {
			return fmt.Errorf("write configPatches[%d] subFilter.name: %w", i, err)
		}

		clusterPath := []string{"patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name"}
		if err := unstructured.SetNestedField(patch, clusterName, clusterPath...); err != nil {
			return fmt.Errorf("write configPatches[%d] grpc cluster_name: %w", i, err)
		}

		configPatches[i] = patch
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

func patchClusterRoleBindingSubject(r *unstructured.Unstructured, name, ns string) error {
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
	subj["name"] = name
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

func replaceHostServiceAndNamespace(host, service, ns string) string {
	parts := strings.SplitN(host, ".", 3)
	if len(parts) >= 2 {
		parts[0] = service
		parts[1] = ns
		return strings.Join(parts, ".")
	}
	return host
}

func addTenantSelectorLabels(r *unstructured.Unstructured, params PlatformParams, fields ...string) error {
	labels, found, err := unstructured.NestedStringMap(r.Object, fields...)
	if err != nil {
		return err
	}
	if !found {
		labels = map[string]string{}
	}
	labels[LabelTenantName] = params.TenantName
	labels[LabelTenantNamespace] = params.TenantNamespace
	return unstructured.SetNestedStringMap(r.Object, labels, fields...)
}

func patchDeploymentSecretVolume(r *unstructured.Unstructured, volumeName, secretName string) error {
	volumes, found, err := unstructured.NestedSlice(r.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("read deployment volumes: %w", err)
	}
	if !found {
		return nil
	}
	for i, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok || volume["name"] != volumeName {
			continue
		}
		secret, ok := volume["secret"].(map[string]any)
		if !ok {
			secret = map[string]any{}
		}
		secret["secretName"] = secretName
		volume["secret"] = secret
		volumes[i] = volume
		return unstructured.SetNestedSlice(r.Object, volumes, "spec", "template", "spec", "volumes")
	}
	return nil
}

func patchHTTPRouteBackendRefs(r *unstructured.Unstructured, serviceName string) error {
	rules, found, err := unstructured.NestedSlice(r.Object, "spec", "rules")
	if err != nil {
		return fmt.Errorf("read HTTPRoute rules: %w", err)
	}
	if !found {
		return nil
	}
	for i, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			return fmt.Errorf("HTTPRoute rules[%d] is not an object", i)
		}
		backendRefs, _ := rule["backendRefs"].([]any)
		for j, rawRef := range backendRefs {
			ref, ok := rawRef.(map[string]any)
			if !ok {
				return fmt.Errorf("HTTPRoute rules[%d].backendRefs[%d] is not an object", i, j)
			}
			ref["name"] = serviceName
			backendRefs[j] = ref
		}
		if backendRefs != nil {
			rule["backendRefs"] = backendRefs
		}
		rules[i] = rule
	}
	return unstructured.SetNestedSlice(r.Object, rules, "spec", "rules")
}

func patchMaaSAPIAuthPolicyURL(r *unstructured.Unstructured, params PlatformParams) error {
	url, found, err := unstructured.NestedString(r.Object, "spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	if err != nil {
		return fmt.Errorf("read scoped AuthPolicy validation URL: %w", err)
	}
	if !found || url == "" {
		return nil
	}
	url = strings.Replace(url, "https://"+MaaSAPIDeploymentName+".", "https://"+params.MaaSAPIName+".", 1)
	if err := unstructured.SetNestedField(r.Object, url, "spec", "rules", "metadata", "apiKeyValidation", "http", "url"); err != nil {
		return fmt.Errorf("write scoped AuthPolicy validation URL: %w", err)
	}
	return nil
}

func patchCleanupCronJobCommand(r *unstructured.Unstructured, serviceName string) error {
	containers, found, err := unstructured.NestedSlice(r.Object, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	if err != nil || !found {
		return errors.New("cleanup containers not found")
	}
	for i, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok || container["name"] != "cleanup" {
			continue
		}
		command, _ := container["command"].([]any)
		for j, rawArg := range command {
			arg, ok := rawArg.(string)
			if !ok {
				continue
			}
			arg = strings.ReplaceAll(arg, "https://"+MaaSAPIDeploymentName+":8443", "https://"+serviceName+":8443")
			arg = strings.ReplaceAll(arg, "http://"+MaaSAPIDeploymentName+":8080", "http://"+serviceName+":8080")
			command[j] = arg
		}
		container["command"] = command
		containers[i] = container
		return unstructured.SetNestedSlice(r.Object, containers, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	}
	return nil
}

func patchCleanupNetworkPolicyEgress(r *unstructured.Unstructured, params PlatformParams) error {
	egress, found, err := unstructured.NestedSlice(r.Object, "spec", "egress")
	if err != nil {
		return fmt.Errorf("read cleanup NetworkPolicy egress: %w", err)
	}
	if !found || len(egress) == 0 {
		return nil
	}
	first, ok := egress[0].(map[string]any)
	if !ok {
		return errors.New("cleanup NetworkPolicy egress[0] is not an object")
	}
	to, _ := first["to"].([]any)
	if len(to) == 0 {
		return nil
	}
	target, ok := to[0].(map[string]any)
	if !ok {
		return errors.New("cleanup NetworkPolicy egress[0].to[0] is not an object")
	}
	podSelector, ok := target["podSelector"].(map[string]any)
	if !ok {
		podSelector = map[string]any{}
	}
	matchLabels, ok := podSelector["matchLabels"].(map[string]any)
	if !ok {
		matchLabels = map[string]any{}
	}
	matchLabels[LabelTenantName] = params.TenantName
	matchLabels[LabelTenantNamespace] = params.TenantNamespace
	podSelector["matchLabels"] = matchLabels
	target["podSelector"] = podSelector
	to[0] = target
	first["to"] = to
	egress[0] = first
	return unstructured.SetNestedSlice(r.Object, egress, "spec", "egress")
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
