package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got)
		assert.Empty(t, warning)
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

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got)
		assert.Empty(t, warning)
	})

	t.Run("neither CR present enables router fallback", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.False(t, got)
		assert.Empty(t, warning)
	})

	forbiddenWasmPlugin := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == gvkWasmPlugin.Kind {
				return apierrors.NewForbidden(schema.GroupResource{Group: gvkWasmPlugin.Group, Resource: "wasmplugins"},
					key.Name, errors.New("cannot get wasmplugins"))
			}
			return apierrors.NewNotFound(schema.GroupResource{Group: GVKEnvoyFilter.Group, Resource: "envoyfilters"}, key.Name)
		},
	}

	t.Run("WasmPlugin forbidden with Kuadrant installed keeps Kuadrant anchors", func(t *testing.T) {
		mapper := meta.NewDefaultRESTMapper(nil)
		mapper.Add(GVKAuthPolicy, meta.RESTScopeNamespace)
		cl := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithInterceptorFuncs(forbiddenWasmPlugin).Build()

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.True(t, got, "a forbidden read must not count as Kuadrant being absent")
		assert.Contains(t, warning, "wasmplugins.extensions.istio.io")
	})

	t.Run("WasmPlugin forbidden without Kuadrant installed enables router fallback", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(forbiddenWasmPlugin).Build()

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.False(t, got)
		assert.Empty(t, warning)
	})

	t.Run("WasmPlugin API not installed enables router fallback", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == gvkWasmPlugin.Kind {
					return &meta.NoKindMatchError{GroupKind: gvkWasmPlugin.GroupKind(), SearchedVersions: []string{gvkWasmPlugin.Version}}
				}
				return apierrors.NewNotFound(schema.GroupResource{Group: GVKEnvoyFilter.Group, Resource: "envoyfilters"}, key.Name)
			},
		}).Build()

		got, warning, err := gatewayHasKuadrantWasmAuth(context.Background(), cl, "openshift-ingress", "maas-default-gateway")
		require.NoError(t, err)
		assert.False(t, got)
		assert.Empty(t, warning)
	})
}

func TestKuadrantGatewayResourceName(t *testing.T) {
	assert.Equal(t, "kuadrant-maas-default-gateway", kuadrantGatewayResourceName("maas-default-gateway"))
}
