package tenantreconcile

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestBuildPlatformParams(t *testing.T) {
	t.Run("if values are not set for optional fields, fall back to defaults", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE", "")
		t.Setenv("RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE", "")
		t.Setenv("RELATED_IMAGE_UBI_MINIMAL_IMAGE", "")

		tenant := &maasv1alpha1.Tenant{
			Spec: maasv1alpha1.TenantSpec{
				GatewayRef: maasv1alpha1.TenantGatewayRef{
					Namespace: "openshift-ingress",
					Name:      "maas-default-gateway",
				},
			},
		}

		got := BuildPlatformParams(tenant, "opendatahub", "https://kubernetes.default.svc")

		assert.Equal(t, "opendatahub", got.AppNamespace)
		assert.Equal(t, "", got.TenantNamespace)
		assert.Equal(t, "", got.TenantName)
		assert.Equal(t, "openshift-ingress", got.GatewayNamespace)
		assert.Equal(t, "maas-default-gateway", got.GatewayName)
		assert.Equal(t, "https://kubernetes.default.svc", got.ClusterAudience)
		assert.Equal(t, DefaultMaaSAPIImage, got.MaaSAPIImage)
		assert.Equal(t, DefaultPayloadProcessingImage, got.PayloadProcessingImage)
		assert.Equal(t, DefaultMaaSAPIKeyCleanupImage, got.MaaSAPIKeyCleanupImage)
		assert.Equal(t, DefaultAPIKeyMaxExpirationDays, got.APIKeyMaxExpirationDays)
	})

	t.Run("if values are set for optional fields, they should prevail", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE", "quay.io/example/maas-api:test")
		t.Setenv("RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE", "quay.io/example/payload:test")
		t.Setenv("RELATED_IMAGE_UBI_MINIMAL_IMAGE", "quay.io/example/cleanup:test")

		maxExpirationDays := int32(45)
		tenant := &maasv1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "tenant-admin",
				Labels: map[string]string{
					LabelAIGatewayTenant: "team-a",
				},
			},
			Spec: maasv1alpha1.TenantSpec{
				GatewayRef: maasv1alpha1.TenantGatewayRef{
					Namespace: "gateway-ns",
					Name:      "gateway-name",
				},
				APIKeys: &maasv1alpha1.TenantAPIKeysConfig{
					MaxExpirationDays: &maxExpirationDays,
				},
			},
		}

		got := BuildPlatformParams(tenant, "tenant-ns", "cluster-audience")

		assert.Equal(t, "tenant-ns", got.AppNamespace)
		assert.Equal(t, "tenant-admin", got.TenantNamespace)
		assert.Equal(t, "team-a", got.TenantName)
		assert.True(t, got.DedicatedMaaSAPI)
		assert.Equal(t, "maas-api-team-a", got.MaaSAPIName)
		assert.Equal(t, "maas-api-route-team-a", got.MaaSAPIRouteName)
		assert.Equal(t, "gateway-ns", got.GatewayNamespace)
		assert.Equal(t, "gateway-name", got.GatewayName)
		assert.Equal(t, "cluster-audience", got.ClusterAudience)
		assert.Equal(t, "quay.io/example/maas-api:test", got.MaaSAPIImage)
		assert.Equal(t, "quay.io/example/payload:test", got.PayloadProcessingImage)
		assert.Equal(t, "quay.io/example/cleanup:test", got.MaaSAPIKeyCleanupImage)
		assert.Equal(t, "45", got.APIKeyMaxExpirationDays)
	})
}

func TestApplyPlatformParamsWithRenderedOverlay(t *testing.T) {
	resources := renderOverlayResources(t, "tenant-ns")
	params := PlatformParams{
		AppNamespace:            "tenant-ns",
		GatewayNamespace:        "gateway-ns",
		GatewayName:             "custom-gateway",
		ClusterAudience:         "openshift-custom",
		MaaSAPIImage:            "quay.io/example/maas-api:test",
		PayloadProcessingImage:  "quay.io/example/payload:test",
		MaaSAPIKeyCleanupImage:  "quay.io/example/cleanup:test",
		APIKeyMaxExpirationDays: "45",
		TenantNamespace:         "team-a-maas",
		TenantName:              "team-a",
	}

	err := applyPlatformParams(logr.Discard(), resources, params)
	require.NoError(t, err)

	maasAPIDeployment := requireResource(t, resources, GVKDeployment, MaaSAPIDeploymentName)
	assert.Equal(t, params.MaaSAPIImage, requireContainerImage(t, maasAPIDeployment, "spec", "template", "spec", "containers"))
	assert.Equal(t, params.GatewayNamespace, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "GATEWAY_NAMESPACE"))
	assert.Equal(t, params.GatewayName, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "GATEWAY_NAME"))
	assert.Equal(t, params.APIKeyMaxExpirationDays, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "API_KEY_MAX_EXPIRATION_DAYS"))
	assert.Equal(t, params.TenantNamespace, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "MAAS_SUBSCRIPTION_NAMESPACE"))
	assert.Equal(t, params.TenantName, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "TENANT_NAME"))

	payloadDeployment := requireResource(t, resources, GVKDeployment, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadDeployment.GetNamespace())
	assert.Equal(t, params.PayloadProcessingImage, requireContainerImage(t, payloadDeployment, "spec", "template", "spec", "containers"))

	if cleanupCronJob := findResource(resources, GVKCronJob, MaaSAPIKeyCleanupCronJobName); cleanupCronJob != nil {
		assert.Equal(t, params.MaaSAPIKeyCleanupImage, requireContainerImage(t, cleanupCronJob, "spec", "jobTemplate", "spec", "template", "spec", "containers"))
	}

	httpRoute := requireResource(t, resources, GVKHTTPRoute, MaaSAPIRouteName)
	parentRefs, found, err := unstructured.NestedSlice(httpRoute.Object, "spec", "parentRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, parentRefs)
	firstParentRef, ok := parentRefs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, params.GatewayNamespace, firstParentRef["namespace"])
	assert.Equal(t, params.GatewayName, firstParentRef["name"])

	authPolicy := requireResource(t, resources, GVKAuthPolicy, MaaSAPIAuthPolicyName)
	audiences, found, err := unstructured.NestedSlice(authPolicy.Object,
		"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, audiences)
	assert.Equal(t, params.ClusterAudience, audiences[0])
	validationURL, found, err := unstructured.NestedString(authPolicy.Object,
		"spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, validationURL, "."+params.AppNamespace+".")

	maasAPIDestinationRule := requireResource(t, resources, GVKDestinationRule, GatewayDestinationRuleName)
	assert.Equal(t, params.GatewayNamespace, maasAPIDestinationRule.GetNamespace())
	maasAPIHost, found, err := unstructured.NestedString(maasAPIDestinationRule.Object, "spec", "host")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, maasAPIHost, "."+params.AppNamespace+".")

	payloadDestinationRule := requireResource(t, resources, GVKDestinationRule, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadDestinationRule.GetNamespace())
	payloadHost, found, err := unstructured.NestedString(payloadDestinationRule.Object, "spec", "host")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, payloadHost, "."+params.GatewayNamespace+".")

	payloadBeforeDestinationRule := requireResource(t, resources, GVKDestinationRule, PayloadPreProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadBeforeDestinationRule.GetNamespace())
	preProcessingHost, found, err := unstructured.NestedString(payloadBeforeDestinationRule.Object, "spec", "host")
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, preProcessingHost, "."+params.GatewayNamespace+".")

	payloadService := requireResource(t, resources, GVKService, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadService.GetNamespace())

	payloadServiceAccount := requireResource(t, resources, GVKServiceAccount, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadServiceAccount.GetNamespace())

	payloadPluginsConfigMap := requireResource(t, resources, GVKConfigMap, PayloadProcessingPluginsConfigMapName)
	assert.Equal(t, params.GatewayNamespace, payloadPluginsConfigMap.GetNamespace())

	payloadEnvoyFilter := requireResource(t, resources, GVKEnvoyFilter, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadEnvoyFilter.GetNamespace())
	targetRefs, found, err := unstructured.NestedSlice(payloadEnvoyFilter.Object, "spec", "targetRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, targetRefs)
	firstTargetRef, ok := targetRefs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, params.GatewayName, firstTargetRef["name"])

	// Verify dual-stage filter chain: configPatches[0]=INSERT_BEFORE, configPatches[1]=INSERT_AFTER.
	configPatches, found, err := unstructured.NestedSlice(payloadEnvoyFilter.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, configPatches, 2, "expected two configPatches (INSERT_BEFORE + INSERT_AFTER)")

	wantAnchor := wasmpluginAnchorName(params.GatewayNamespace, params.GatewayName)
	wantBeforeCluster := grpcClusterName(PayloadPreProcessingName, params.GatewayNamespace, 9004)
	wantAfterCluster := grpcClusterName(PayloadProcessingName, params.GatewayNamespace, 9004)
	wantOps := []string{"INSERT_BEFORE", "INSERT_AFTER"}
	wantClusters := []string{wantBeforeCluster, wantAfterCluster}

	for i, raw := range configPatches {
		cp, ok := raw.(map[string]any)
		require.True(t, ok, "configPatches[%d] should be a map", i)

		op, _, _ := unstructured.NestedString(cp, "patch", "operation")
		assert.Equal(t, wantOps[i], op, "configPatches[%d] operation", i)

		anchor, _, _ := unstructured.NestedString(cp, "match", "listener", "filterChain", "filter", "subFilter", "name")
		assert.Equal(t, wantAnchor, anchor, "configPatches[%d] subFilter.name", i)

		cluster, _, _ := unstructured.NestedString(cp, "patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name")
		assert.Equal(t, wantClusters[i], cluster, "configPatches[%d] grpc cluster_name", i)
	}

	// Verify payload-pre-processing Deployment and Service are present and namespaced correctly.
	payloadBeforeDeployment := requireResource(t, resources, GVKDeployment, PayloadPreProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadBeforeDeployment.GetNamespace())
	assert.Equal(t, params.PayloadProcessingImage, requireContainerImage(t, payloadBeforeDeployment, "spec", "template", "spec", "containers"))

	payloadBeforeService := requireResource(t, resources, GVKService, PayloadPreProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadBeforeService.GetNamespace())

	payloadClusterRoleBinding := requireResource(t, resources, GVKClusterRoleBinding, PayloadProcessingReaderClusterRoleBindingName)
	subjects, found, err := unstructured.NestedSlice(payloadClusterRoleBinding.Object, "subjects")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, subjects)
	firstSubject, ok := subjects[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, params.GatewayNamespace, firstSubject["namespace"])
}

func TestApplyPlatformParamsScopesDedicatedMaaSAPI(t *testing.T) {
	resources := renderOverlayResources(t, "redhat-ai-gateway-infra")
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "team-a-maas",
			Labels: map[string]string{
				LabelAIGatewayTenant: "team-a",
			},
		},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "team-a-gateway",
			},
		},
	}
	params := BuildPlatformParams(tenant, "redhat-ai-gateway-infra", "cluster-audience")

	err := applyPlatformParams(logr.Discard(), resources, params)
	require.NoError(t, err)

	deployment := requireResource(t, resources, GVKDeployment, params.MaaSAPIName)
	assert.Equal(t, params.MaaSAPIName, nestedString(t, deployment, "spec", "template", "spec", "serviceAccountName"))
	podLabels := nestedStringMap(t, deployment, "spec", "template", "metadata", "labels")
	assert.Equal(t, "team-a", podLabels[LabelTenantName])
	assert.Equal(t, "team-a-maas", podLabels[LabelTenantNamespace])
	assert.Equal(t, "team-a", requireEnvVarValue(t, deployment, "maas-api", "TENANT_NAME"))
	assert.Equal(t, "team-a-maas", requireEnvVarValue(t, deployment, "maas-api", "MAAS_SUBSCRIPTION_NAMESPACE"))

	service := requireResource(t, resources, GVKService, params.MaaSAPIName)
	serviceSelector := nestedStringMap(t, service, "spec", "selector")
	assert.Equal(t, "team-a", serviceSelector[LabelTenantName])
	assert.Equal(t, params.MaaSAPIServiceCertSecretName, service.GetAnnotations()["service.beta.openshift.io/serving-cert-secret-name"])

	httpRoute := requireResource(t, resources, GVKHTTPRoute, params.MaaSAPIRouteName)
	rules, found, err := unstructured.NestedSlice(httpRoute.Object, "spec", "rules")
	require.NoError(t, err)
	require.True(t, found)
	firstRule, ok := rules[0].(map[string]any)
	require.True(t, ok)
	backendRefs, ok := firstRule["backendRefs"].([]any)
	require.True(t, ok)
	firstBackend, ok := backendRefs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, params.MaaSAPIName, firstBackend["name"])

	authPolicy := requireResource(t, resources, GVKAuthPolicy, params.MaaSAPIAuthPolicyName)
	assert.Equal(t, params.MaaSAPIRouteName, nestedString(t, authPolicy, "spec", "targetRef", "name"))
	validationURL := nestedString(t, authPolicy, "spec", "rules", "metadata", "apiKeyValidation", "http", "url")
	assert.Contains(t, validationURL, "https://"+params.MaaSAPIName+".redhat-ai-gateway-infra.svc.cluster.local")

	cleanup := requireResource(t, resources, GVKCronJob, params.MaaSAPIKeyCleanupCronJobName)
	assert.Equal(t, params.MaaSAPIName, nestedString(t, cleanup, "spec", "jobTemplate", "spec", "template", "spec", "serviceAccountName"))
	containers, found, err := unstructured.NestedSlice(cleanup.Object, "spec", "jobTemplate", "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	cleanupContainer, ok := containers[0].(map[string]any)
	require.True(t, ok)
	command, ok := cleanupContainer["command"].([]any)
	require.True(t, ok)
	cleanupCommand, ok := command[len(command)-1].(string)
	require.True(t, ok)
	assert.Contains(t, cleanupCommand, "https://"+params.MaaSAPIName+":8443/internal/v1/api-keys/cleanup")
}

func renderOverlayResources(t *testing.T, appNamespace string) []unstructured.Unstructured {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)

	overlayDir := filepath.Clean(filepath.Join(
		filepath.Dir(currentFile),
		"..", "..", "..", "..",
		"maas-api", "deploy", "overlays", "odh",
	))

	resources, err := RenderKustomize(overlayDir, appNamespace)
	require.NoError(t, err)

	return resources
}

func requireResource(t *testing.T, resources []unstructured.Unstructured, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()

	if r := findResource(resources, gvk, name); r != nil {
		return r
	}

	t.Fatalf("resource %s %q not found", gvk.String(), name)
	return nil
}

func findResource(resources []unstructured.Unstructured, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	for i := range resources {
		if resources[i].GroupVersionKind() == gvk && resources[i].GetName() == name {
			return &resources[i]
		}
	}
	return nil
}

func requireContainerImage(t *testing.T, r *unstructured.Unstructured, fields ...string) string {
	t.Helper()

	containers, found, err := unstructured.NestedSlice(r.Object, fields...)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, containers)

	firstContainer, ok := containers[0].(map[string]any)
	require.True(t, ok)

	image, ok := firstContainer["image"].(string)
	require.True(t, ok)
	return image
}

func requireEnvVarValue(t *testing.T, r *unstructured.Unstructured, containerName, envName string) string {
	t.Helper()

	containers, found, err := unstructured.NestedSlice(r.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)

	for _, c := range containers {
		containerMap, ok := c.(map[string]any)
		require.True(t, ok)
		if containerMap["name"] != containerName {
			continue
		}

		envSlice, _ := containerMap["env"].([]any)
		for _, e := range envSlice {
			envMap, ok := e.(map[string]any)
			require.True(t, ok)
			if envMap["name"] == envName {
				value, ok := envMap["value"].(string)
				require.True(t, ok)
				return value
			}
		}
	}

	t.Fatalf("env var %q not found in container %q", envName, containerName)
	return ""
}

func nestedString(t *testing.T, r *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(r.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "nested string %v not found", fields)
	return value
}

func nestedStringMap(t *testing.T, r *unstructured.Unstructured, fields ...string) map[string]string {
	t.Helper()
	value, found, err := unstructured.NestedStringMap(r.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "nested string map %v not found", fields)
	return value
}
