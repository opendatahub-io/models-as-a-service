package tenantreconcile

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

		platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Namespace: "openshift-ingress",
			Name:      "maas-default-gateway",
		}}
		got, err := BuildPlatformParams(tenant, platformContext, "opendatahub", "https://kubernetes.default.svc", logr.Discard())
		assert.NoError(t, err)

		assert.Equal(t, "opendatahub", got.AppNamespace)
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

		platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Namespace: "gateway-ns",
			Name:      "gateway-name",
		}}
		got, err := BuildPlatformParams(tenant, platformContext, "tenant-ns", "cluster-audience", logr.Discard())
		assert.NoError(t, err)

		assert.Equal(t, "tenant-ns", got.AppNamespace)
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
	}

	err := applyPlatformParams(logr.Discard(), resources, params)
	require.NoError(t, err)

	tenantID := params.TenantIdentifier
	maasAPIDeployment := requireResource(t, resources, GVKDeployment, MaaSAPIDeploymentName(tenantID))
	assert.Equal(t, params.MaaSAPIImage, requireContainerImage(t, maasAPIDeployment, "spec", "template", "spec", "containers"))
	assert.Equal(t, params.GatewayNamespace, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "GATEWAY_NAMESPACE"))
	assert.Equal(t, params.GatewayName, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "GATEWAY_NAME"))
	assert.Equal(t, params.APIKeyMaxExpirationDays, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "API_KEY_MAX_EXPIRATION_DAYS"))
	// TENANT_NAME is "models-as-a-service" for default tenant (empty tenantID), otherwise tenantID
	expectedTenantName := tenantID
	if expectedTenantName == "" {
		expectedTenantName = "models-as-a-service"
	}
	assert.Equal(t, expectedTenantName, requireEnvVarValue(t, maasAPIDeployment, "maas-api", "TENANT_NAME"))

	payloadDeployment := requireResource(t, resources, GVKDeployment, PayloadProcessingName)
	assert.Equal(t, params.GatewayNamespace, payloadDeployment.GetNamespace())
	assert.Equal(t, params.PayloadProcessingImage, requireContainerImage(t, payloadDeployment, "spec", "template", "spec", "containers"))

	if cleanupCronJob := findResource(resources, GVKCronJob, MaaSAPIKeyCleanupCronJobName(tenantID)); cleanupCronJob != nil {
		assert.Equal(t, params.MaaSAPIKeyCleanupImage, requireContainerImage(t, cleanupCronJob, "spec", "jobTemplate", "spec", "template", "spec", "containers"))
	}

	httpRoute := requireResource(t, resources, GVKHTTPRoute, MaaSAPIRouteName(tenantID))
	parentRefs, found, err := unstructured.NestedSlice(httpRoute.Object, "spec", "parentRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, parentRefs)
	firstParentRef, ok := parentRefs[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, params.GatewayNamespace, firstParentRef["namespace"])
	assert.Equal(t, params.GatewayName, firstParentRef["name"])

	// maas-api-auth-policy is no longer rendered by kustomize; auth for maas-api-route
	// is handled by the singleton maas-gateway-auth AuthPolicy (managed by the controller).

	maasAPIDestinationRule := requireResource(t, resources, GVKDestinationRule, GatewayDestinationRuleName(tenantID))
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

	// Verify dual-stage filter chain with dual anchors:
	//   [0..1] WasmPlugin (ODH/community Kuadrant), [2..3] wasm filter (RHCL 1.4),
	//   [4..7] per-route disable MERGE on maas-api-route rules 0–3.
	configPatches, found, err := unstructured.NestedSlice(payloadEnvoyFilter.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, configPatches, 8, "expected eight configPatches (4x filter insert + 4x MERGE)")

	wantWasmPluginAnchor := wasmpluginAnchorName(params.GatewayNamespace, params.GatewayName)
	wantBeforeCluster := grpcClusterName(PayloadPreProcessingName, params.GatewayNamespace, 9004)
	wantAfterCluster := grpcClusterName(PayloadProcessingName, params.GatewayNamespace, 9004)
	wantOps := []string{"INSERT_BEFORE", "INSERT_AFTER", "INSERT_BEFORE", "INSERT_AFTER"}
	wantAnchors := []string{wantWasmPluginAnchor, wantWasmPluginAnchor, rhclWasmFilterName, rhclWasmFilterName}
	wantClusters := []string{wantBeforeCluster, wantAfterCluster, wantBeforeCluster, wantAfterCluster}

	for i, raw := range configPatches[:4] {
		cp, ok := raw.(map[string]any)
		require.True(t, ok, "configPatches[%d] should be a map", i)

		op, _, _ := unstructured.NestedString(cp, "patch", "operation")
		assert.Equal(t, wantOps[i], op, "configPatches[%d] operation", i)

		anchor, _, _ := unstructured.NestedString(cp, "match", "listener", "filterChain", "filter", "subFilter", "name")
		assert.Equal(t, wantAnchors[i], anchor, "configPatches[%d] subFilter.name", i)

		cluster, _, _ := unstructured.NestedString(cp, "patch", "value", "typed_config", "grpc_service", "envoy_grpc", "cluster_name")
		assert.Equal(t, wantClusters[i], cluster, "configPatches[%d] grpc cluster_name", i)
	}

	// Verify per-route ext_proc disable on maas-api-route rules 0–3.
	for i := 4; i < 8; i++ {
		cp, ok := configPatches[i].(map[string]any)
		require.True(t, ok, "configPatches[%d] should be a map", i)

		op, _, _ := unstructured.NestedString(cp, "patch", "operation")
		assert.Equal(t, "MERGE", op, "configPatches[%d] operation", i)

		routeName, _, _ := unstructured.NestedString(cp, "match", "routeConfiguration", "vhost", "route", "name")
		wantRouteName := fmt.Sprintf("%s.%s.%d", params.AppNamespace, MaaSAPIRouteName(params.TenantIdentifier), i-4)
		assert.Equal(t, wantRouteName, routeName, "configPatches[%d] route name", i)

		disabled, found, err := unstructured.NestedBool(cp, "patch", "value", "typed_per_filter_config", "envoy.filters.http.ext_proc.ipp-pre", "disabled")
		require.NoError(t, err, "configPatches[%d] ipp-pre disabled field", i)
		require.True(t, found, "configPatches[%d] ipp-pre disabled field should exist", i)
		assert.True(t, disabled, "configPatches[%d] ipp-pre should be disabled", i)

		ippDisabled, found, err := unstructured.NestedBool(cp, "patch", "value", "typed_per_filter_config", "envoy.filters.http.ext_proc.ipp", "disabled")
		require.NoError(t, err, "configPatches[%d] ipp disabled field", i)
		require.True(t, found, "configPatches[%d] ipp disabled field should exist", i)
		assert.True(t, ippDisabled, "configPatches[%d] ipp should be disabled", i)
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
