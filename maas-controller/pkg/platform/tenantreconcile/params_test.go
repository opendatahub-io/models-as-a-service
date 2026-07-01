package tenantreconcile

import (
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
		assert.Equal(t, DefaultMaaSAPIKeyCleanupImage, got.MaaSAPIKeyCleanupImage)
		assert.Equal(t, DefaultAPIKeyMaxExpirationDays, got.APIKeyMaxExpirationDays)
	})

	t.Run("if values are set for optional fields, they should prevail", func(t *testing.T) {
		t.Setenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE", "quay.io/example/maas-api:test")
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
