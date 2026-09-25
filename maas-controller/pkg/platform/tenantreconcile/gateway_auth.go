package tenantreconcile

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var gvkWasmPlugin = schema.GroupVersionKind{
	Group:   "extensions.istio.io",
	Version: "v1alpha1",
	Kind:    "WasmPlugin",
}

func kuadrantGatewayResourceName(gatewayName string) string {
	return fmt.Sprintf("kuadrant-%s", gatewayName)
}

// gatewayHasKuadrantWasmAuth reports whether Kuadrant auth is wired on the gateway via
// RHCL EnvoyFilter (envoy.filters.http.wasm) or ODH/community WasmPlugin. A non-empty
// warning means this could not be verified and Kuadrant was assumed present.
func gatewayHasKuadrantWasmAuth(ctx context.Context, c client.Client, gatewayNamespace, gatewayName string) (bool, string, error) {
	name := kuadrantGatewayResourceName(gatewayName)
	key := types.NamespacedName{Namespace: gatewayNamespace, Name: name}

	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(GVKEnvoyFilter)
	if err := c.Get(ctx, key, ef); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("get EnvoyFilter %s: %w", key, err)
		}
	} else {
		return true, "", nil
	}

	wp := &unstructured.Unstructured{}
	wp.SetGroupVersionKind(gvkWasmPlugin)
	err := c.Get(ctx, key, wp)
	switch {
	case err == nil:
		return true, "", nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// No kuadrant-{gateway} WasmPlugin CR (or no WasmPlugin API at all): use the
		// router-anchored ext_proc fallback.
		return false, "", nil
	case apierrors.IsForbidden(err):
		// Without permission to check, fall back to router anchors only when Kuadrant is
		// not installed at all; otherwise keep the Kuadrant anchors rather than assume it
		// is absent. Expected only until the parent operator grants get.
		kuadrantInstalled, gvkErr := IsGVKAvailable(c, GVKAuthPolicy)
		if gvkErr != nil {
			return false, "", fmt.Errorf("check Kuadrant AuthPolicy API: %w", gvkErr)
		}
		if !kuadrantInstalled {
			return false, "", nil
		}
		return true, fmt.Sprintf("cannot get WasmPlugin %s; keeping Kuadrant-anchored payload processing "+
			"until maas-controller may get wasmplugins.extensions.istio.io", key), nil
	default:
		return false, "", fmt.Errorf("get WasmPlugin %s: %w", key, err)
	}
}
