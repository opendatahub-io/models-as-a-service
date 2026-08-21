package tenantreconcile

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// RHCL EnvoyFilter (envoy.filters.http.wasm) or ODH/community WasmPlugin.
func gatewayHasKuadrantWasmAuth(ctx context.Context, c client.Client, gatewayNamespace, gatewayName string) (bool, error) {
	name := kuadrantGatewayResourceName(gatewayName)
	key := types.NamespacedName{Namespace: gatewayNamespace, Name: name}

	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(GVKEnvoyFilter)
	if err := c.Get(ctx, key, ef); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get EnvoyFilter %s: %w", key, err)
		}
	} else {
		return true, nil
	}

	wp := &unstructured.Unstructured{}
	wp.SetGroupVersionKind(gvkWasmPlugin)
	if err := c.Get(ctx, key, wp); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			// No kuadrant-{gateway} WasmPlugin CR (or RBAC hides it). Use router-anchored
			// ext_proc fallback; missing wasmplugin RBAC must not block reconcile.
			return false, nil
		}
		return false, fmt.Errorf("get WasmPlugin %s: %w", key, err)
	}
	return true, nil
}
