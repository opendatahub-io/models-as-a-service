package tenantreconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	envoyCompressorFilter   = "envoy.filters.http.compressor"
	envoyDecompressorFilter = "envoy.filters.http.decompressor"
	envoyRouterFilter       = "envoy.filters.http.router"
)

func TestGatewayContentEncodingEnvoyFilterName(t *testing.T) {
	assert.Equal(t, "maas-gateway-encoding", GatewayContentEncodingEnvoyFilterName(""))
	assert.Equal(t, "maas-gateway-encoding-redteam", GatewayContentEncodingEnvoyFilterName("redteam"))

	// AITenant names are capped at 41 characters so every per-tenant name fits in 63.
	longest := strings.Repeat("a", 41)
	assert.LessOrEqual(t, len(GatewayContentEncodingEnvoyFilterName(longest)), 63)
}

// The compressor and decompressor only work around every other HTTP filter, so their
// EnvoyFilter has to be applied after every other one this repo ships. Istio orders equal
// priorities by creation time, which differs between a fresh install and an upgrade.
func TestGatewayContentEncodingEnvoyFilterIsAppliedLast(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	manifests := os.DirFS(filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "..", "deployment"))

	priorities := map[string]int64{}
	err := fs.WalkDir(manifests, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := fs.ReadFile(manifests, path)
		if err != nil || !bytes.Contains(data, []byte("kind: EnvoyFilter")) {
			return err
		}
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var doc struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Priority int64 `json:"priority"`
				} `json:"spec"`
			}
			err = decoder.Decode(&doc)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if doc.Kind == "EnvoyFilter" {
				priorities[doc.Metadata.Name] = doc.Spec.Priority
			}
		}
	})
	require.NoError(t, err)

	// The manifest, rendered without the controller, has to say what the controller writes.
	require.Contains(t, priorities, GatewayContentEncodingEnvoyFilterBaseName)
	assert.Equal(t, GatewayContentEncodingEnvoyFilterPriority, priorities[GatewayContentEncodingEnvoyFilterBaseName])
	delete(priorities, GatewayContentEncodingEnvoyFilterBaseName)

	// The two that insert HTTP filters first and in front of the router.
	require.Contains(t, priorities, PayloadProcessingName)
	require.Contains(t, priorities, baseUsageLogsEnvoyFilterName)
	for name, priority := range priorities {
		assert.Less(t, priority, GatewayContentEncodingEnvoyFilterPriority,
			"EnvoyFilter %s needs a lower priority than %s", name, GatewayContentEncodingEnvoyFilterBaseName)
	}
}

func TestGatewayContentEncodingEnvoyFilterRendered(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		wantName string
	}{
		{name: "default tenant", tenantID: "", wantName: "maas-gateway-encoding"},
		{name: "AITenant", tenantID: "redteam", wantName: "maas-gateway-encoding-redteam"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources := renderOverlayResources(t, "tenant-ns")
			params := PlatformParams{ //nolint:gosec // APIKeyMaxExpirationDays is a duration setting, not a secret
				AppNamespace:            "tenant-ns",
				ControllerNamespace:     "controller-ns",
				GatewayNamespace:        "gateway-ns",
				GatewayName:             "custom-gateway",
				TenantIdentifier:        tt.tenantID,
				SubscriptionNamespace:   "tenant-ns",
				MaaSAPIImage:            "quay.io/example/maas-api:test",
				PayloadProcessingImage:  "quay.io/example/payload:test",
				MaaSAPIKeyCleanupImage:  "quay.io/example/cleanup:test",
				APIKeyMaxExpirationDays: "45",
			}
			require.NoError(t, applyPlatformParams(logr.Discard(), resources, params))

			filter := requireResource(t, resources, GVKEnvoyFilter, tt.wantName)
			assert.Equal(t, "gateway-ns", filter.GetNamespace())

			labels, found, err := unstructured.NestedStringMap(filter.Object, "spec", "workloadSelector", "labels")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, map[string]string{"gateway.networking.k8s.io/gateway-name": "custom-gateway"}, labels)

			priority, found, err := unstructured.NestedInt64(filter.Object, "spec", "priority")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, GatewayContentEncodingEnvoyFilterPriority, priority)

			// Against what is applied, not the manifests: the controller writes the
			// payload-processing priority itself.
			others := 0
			for i := range resources {
				other := &resources[i]
				if other.GroupVersionKind() != GVKEnvoyFilter || other.GetName() == tt.wantName {
					continue
				}
				others++
				otherPriority, _, err := unstructured.NestedInt64(other.Object, "spec", "priority")
				require.NoError(t, err)
				assert.Less(t, otherPriority, priority,
					"EnvoyFilter %s would be applied after %s", other.GetName(), tt.wantName)
			}
			assert.NotZero(t, others, "no other EnvoyFilter was rendered to compare with")

			patches, found, err := unstructured.NestedSlice(filter.Object, "spec", "configPatches")
			require.NoError(t, err)
			require.True(t, found)
			require.Len(t, patches, 2)

			// The compressor goes first in the chain, so it is the last filter to see a
			// response. It must not be anchored on a filter whose name another component owns.
			compressor := requireHTTPFilterPatch(t, patches, envoyCompressorFilter)
			assert.Equal(t, "INSERT_FIRST", nestedString(t, compressor, "patch", "operation"))
			_, anchored, err := unstructured.NestedMap(compressor, "match", "listener", "filterChain", "filter", "subFilter")
			require.NoError(t, err)
			assert.False(t, anchored, "INSERT_FIRST must not depend on another filter's name")
			assert.Equal(t,
				"type.googleapis.com/envoy.extensions.compression.gzip.compressor.v3.Gzip",
				nestedString(t, compressor, "patch", "value", "typed_config", "compressor_library", "typed_config", "@type"))
			// Without this the provider still sees the client's Accept-Encoding and may
			// answer with an encoding the decompressor cannot decode.
			assert.True(t, nestedBool(t, compressor,
				"patch", "value", "typed_config", "response_direction_config", "remove_accept_encoding_header"))

			// The decompressor goes right in front of the router, so it is the first filter
			// to see a response.
			decompressor := requireHTTPFilterPatch(t, patches, envoyDecompressorFilter)
			assert.Equal(t, "INSERT_BEFORE", nestedString(t, decompressor, "patch", "operation"))
			assert.Equal(t, envoyRouterFilter,
				nestedString(t, decompressor, "match", "listener", "filterChain", "filter", "subFilter", "name"))
			assert.Equal(t,
				"type.googleapis.com/envoy.extensions.compression.gzip.decompressor.v3.Gzip",
				nestedString(t, decompressor, "patch", "value", "typed_config", "decompressor_library", "typed_config", "@type"))
			assert.True(t, nestedBool(t, decompressor,
				"patch", "value", "typed_config", "response_direction_config", "common_config", "ignore_no_transform_header"))
			// Envoy's default of 100 truncates a long, highly repetitive completion.
			inflateRatio, found, err := unstructured.NestedInt64(decompressor,
				"patch", "value", "typed_config", "decompressor_library", "typed_config", "max_inflate_ratio")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, int64(1032), inflateRatio)
			// Request bodies stay as the client sent them.
			assert.False(t, nestedBool(t, decompressor,
				"patch", "value", "typed_config", "request_direction_config", "common_config", "enabled", "default_value"))
		})
	}
}

// ai-gateway-controller owns the ext_proc EnvoyFilter of a praxis tenant, but the token
// counter is Kuadrant's on every tenant: the compressor/decompressor is not an IPP resource.
func TestPostRender_KeepsGatewayContentEncodingForPraxisTenant(t *testing.T) {
	assert.False(t, isIPPResource(GVKEnvoyFilter, GatewayContentEncodingEnvoyFilterBaseName))

	rendered := renderOverlayResources(t, "ai-tenant-praxis")
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "ai-tenant-praxis",
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        "praxis-team",
			},
		},
	}
	params := PlatformParams{ //nolint:gosec // APIKeyMaxExpirationDays is a duration setting, not a secret
		AppNamespace:            "ai-tenant-praxis",
		ControllerNamespace:     "controller-ns",
		GatewayNamespace:        "openshift-ingress",
		GatewayName:             "praxis-gateway",
		ClusterAudience:         "openshift-custom",
		TenantIdentifier:        "praxis-team",
		SubscriptionNamespace:   "ai-tenant-praxis",
		MaaSAPIImage:            "quay.io/example/maas-api:test",
		PayloadProcessingImage:  "quay.io/example/payload:test",
		MaaSAPIKeyCleanupImage:  "quay.io/example/cleanup:test",
		APIKeyMaxExpirationDays: "45",
		SkipIPP:                 true,
	}

	resources, err := PostRender(context.Background(), logr.Discard(), tenant, rendered, params)
	require.NoError(t, err)

	filter := requireResource(t, resources, GVKEnvoyFilter, "maas-gateway-encoding-praxis-team")
	assert.Equal(t, "openshift-ingress", filter.GetNamespace())
	labels, found, err := unstructured.NestedStringMap(filter.Object, "spec", "workloadSelector", "labels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "praxis-gateway", labels["gateway.networking.k8s.io/gateway-name"])
}

// requireHTTPFilterPatch returns the HTTP_FILTER patch that inserts the named filter on
// gateway listeners.
func requireHTTPFilterPatch(t *testing.T, patches []any, filterName string) map[string]any {
	t.Helper()

	for _, item := range patches {
		patch, ok := item.(map[string]any)
		require.True(t, ok)
		name, _, err := unstructured.NestedString(patch, "patch", "value", "name")
		require.NoError(t, err)
		if name != filterName {
			continue
		}
		assert.Equal(t, "HTTP_FILTER", nestedString(t, patch, "applyTo"))
		assert.Equal(t, "GATEWAY", nestedString(t, patch, "match", "context"))
		assert.Equal(t, "envoy.filters.network.http_connection_manager",
			nestedString(t, patch, "match", "listener", "filterChain", "filter", "name"))
		return patch
	}
	require.Failf(t, "missing patch", "no configPatch inserts %s", filterName)
	return nil
}

func nestedString(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()

	value, found, err := unstructured.NestedString(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "%s is not set", strings.Join(fields, "."))
	return value
}

func nestedBool(t *testing.T, obj map[string]any, fields ...string) bool {
	t.Helper()

	value, found, err := unstructured.NestedBool(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "%s is not set", strings.Join(fields, "."))
	return value
}
