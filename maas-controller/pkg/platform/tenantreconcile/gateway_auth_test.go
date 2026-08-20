package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGatewayHasKuadrantWasmAuth(t *testing.T) {
	scheme := runtime.NewScheme()

	t.Run("EnvoyFilter present", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "networking.istio.io/v1alpha3",
					"kind":       "EnvoyFilter",
					"metadata": map[string]any{
						"name":      "kuadrant-maas-default-gateway",
						"namespace": "openshift-ingress",
					},
				},
			},
		).Build()

		got, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("WasmPlugin present when EnvoyFilter absent", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "extensions.istio.io/v1alpha1",
					"kind":       "WasmPlugin",
					"metadata": map[string]any{
						"name":      "kuadrant-maas-default-gateway",
						"namespace": "openshift-ingress",
					},
				},
			},
		).Build()

		got, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("neither CR present assumes RHCL wasm path", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()

		got, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got)
	})
}

func TestKuadrantGatewayResourceName(t *testing.T) {
	assert.Equal(t, "kuadrant-maas-default-gateway", kuadrantGatewayResourceName("maas-default-gateway"))
}
