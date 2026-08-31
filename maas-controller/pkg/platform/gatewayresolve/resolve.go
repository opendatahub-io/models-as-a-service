package gatewayresolve

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// ForNamespace resolves the Gateway API parent reference for resources in tenantNamespace.
// AITenant-managed namespaces are resolved via namespace labels → AITenant.status.gatewayRef.
// Unmanaged namespaces fall back to legacy Tenant.spec.gatewayRef when present, otherwise
// controller-configured default gateway flags.
func ForNamespace(
	ctx context.Context,
	c client.Reader,
	tenantNamespace string,
	aitenantNamespace string,
	defaultTenantNamespace string,
	fallbackGatewayName string,
	fallbackGatewayNamespace string,
	discoveryEnabled bool,
) (maasv1alpha1.TenantGatewayRef, error) {
	if aitenantNamespace == "" {
		aitenantNamespace = tenantreconcile.DefaultAITenantNamespace
	}
	fallback := fallbackRef(fallbackGatewayName, fallbackGatewayNamespace)

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: tenantNamespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			if tenantNamespace == defaultTenantNamespace || !discoveryEnabled {
				return fallback, nil
			}
			return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("namespace %s not found", tenantNamespace)
		}
		return maasv1alpha1.TenantGatewayRef{}, err
	}

	if isAITenantManagedNamespace(ns.Labels) {
		ref, err := gatewayFromAITenant(ctx, c, ns.Labels, aitenantNamespace)
		if err != nil {
			if tenantNamespace == defaultTenantNamespace {
				return fallback, nil
			}
			return maasv1alpha1.TenantGatewayRef{}, err
		}
		return ref, nil
	}

	if ref, err := gatewayFromLegacyTenant(ctx, c, tenantNamespace); err != nil {
		return maasv1alpha1.TenantGatewayRef{}, err
	} else if ref.Name != "" && ref.Namespace != "" {
		return ref, nil
	}

	if tenantNamespace == defaultTenantNamespace || !discoveryEnabled {
		return fallback, nil
	}
	if namespaceHasTenantDiscoveryLabel(ns.Labels) {
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf(
			"namespace %s is tenant-discovered but not AITenant-managed (missing %s=true)",
			tenantNamespace, tenantreconcile.LabelManagedByAITenant,
		)
	}
	return fallback, nil
}

func isAITenantManagedNamespace(labels map[string]string) bool {
	return labels != nil && labels[tenantreconcile.LabelManagedByAITenant] == "true"
}

func gatewayFromAITenant(
	ctx context.Context,
	c client.Reader,
	labels map[string]string,
	aitenantNamespace string,
) (maasv1alpha1.TenantGatewayRef, error) {
	tenantName := labels[tenantreconcile.LabelTenantName]
	if tenantName == "" {
		tenantName = labels[tenantreconcile.LabelAIGatewayTenant]
	}
	if tenantName == "" {
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant-managed namespace is missing %s", tenantreconcile.LabelTenantName)
	}

	aitenant := &maasv1alpha1.AITenant{}
	key := client.ObjectKey{Name: tenantName, Namespace: aitenantNamespace}
	if err := c.Get(ctx, key, aitenant); err != nil {
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("get AITenant %s/%s: %w", key.Namespace, key.Name, err)
	}

	ref := aitenant.Status.GatewayRef
	if ref.Name == "" || ref.Namespace == "" {
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant %s/%s status.gatewayRef is not ready", key.Namespace, key.Name)
	}
	return ref, nil
}

func gatewayFromLegacyTenant(ctx context.Context, c client.Reader, tenantNamespace string) (maasv1alpha1.TenantGatewayRef, error) {
	legacy := &maasv1alpha1.Tenant{}
	key := client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNamespace}
	if err := c.Get(ctx, key, legacy); err != nil {
		if apierrors.IsNotFound(err) {
			return maasv1alpha1.TenantGatewayRef{}, nil
		}
		return maasv1alpha1.TenantGatewayRef{}, err
	}

	ref := legacy.Spec.GatewayRef
	if ref.Name == "" && ref.Namespace == "" {
		return maasv1alpha1.TenantGatewayRef{}, nil
	}
	if ref.Name == "" || ref.Namespace == "" {
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("tenant %s/%s spec.gatewayRef must set both name and namespace", legacy.Namespace, legacy.Name)
	}
	return ref, nil
}

func namespaceHasTenantDiscoveryLabel(labels map[string]string) bool {
	return labels[tenantreconcile.LabelAIGatewayTenant] != "" ||
		labels[tenantreconcile.LabelManagedByAITenant] == "true"
}

func fallbackRef(name, namespace string) maasv1alpha1.TenantGatewayRef {
	if name == "" || namespace == "" {
		return maasv1alpha1.TenantGatewayRef{}
	}
	return maasv1alpha1.TenantGatewayRef{Name: name, Namespace: namespace}
}
