package tenantreconcile

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	// AnnotationAITenantName identifies the AITenant that owns an AITenant-managed
	// namespace-local MaasTenantConfig/default-tenant object.
	AnnotationAITenantName = "maas.opendatahub.io/aitenant-name"

	// AnnotationAITenantNamespace identifies the namespace of the owning AITenant.
	AnnotationAITenantNamespace = "maas.opendatahub.io/aitenant-namespace"

	tenantNamespacePrefix = "ai-tenant-"
)

// PlatformContext contains platform-derived tenant values used when rendering
// and reconciling tenant infrastructure. AITenant-managed tenants receive these
// values from AITenant; legacy tenants receive them from Tenant spec/defaults.
type PlatformContext struct {
	GatewayRef   maasv1alpha1.TenantGatewayRef
	ExternalOIDC *maasv1alpha1.TenantExternalOIDCConfig
	Source       string
}

// ResolvePlatformContext resolves gateway and OIDC values for a tenant config object.
//
// AITenant-managed configs use their owning AITenant as the source of platform
// context. Legacy/unmanaged Tenant objects preserve the previous behavior and
// use Tenant.spec values for migration compatibility.
func ResolvePlatformContext(ctx context.Context, c client.Reader, tenant client.Object, fallbackGatewayRef maasv1alpha1.TenantGatewayRef) (PlatformContext, error) {
	if tenant == nil {
		return PlatformContext{GatewayRef: fallbackGatewayRef, Source: "default"}, nil
	}

	if isAITenantManagedTenantConfig(tenant) {
		return resolveAITenantPlatformContext(ctx, c, tenant)
	}

	if legacy, ok := tenant.(*maasv1alpha1.Tenant); ok {
		ref := legacy.Spec.GatewayRef
		switch {
		case ref.Name == "" && ref.Namespace == "":
			ref = fallbackGatewayRef
		case ref.Name == "" || ref.Namespace == "":
			return PlatformContext{}, fmt.Errorf("tenant %s/%s spec.gatewayRef must set both name and namespace", legacy.Namespace, legacy.Name)
		}

		return PlatformContext{
			GatewayRef:   ref,
			ExternalOIDC: legacy.Spec.ExternalOIDC.DeepCopy(),
			Source:       "legacy-tenant-spec",
		}, nil
	}

	return PlatformContext{
		GatewayRef: fallbackGatewayRef,
		Source:     "tenant-config",
	}, nil
}

func resolveAITenantPlatformContext(ctx context.Context, c client.Reader, tenant client.Object) (PlatformContext, error) {
	tenantName := tenant.GetLabels()[LabelTenantName]
	if tenantName == "" {
		return PlatformContext{}, fmt.Errorf("AITenant-managed tenant config %s/%s is missing %s", tenant.GetNamespace(), tenant.GetName(), LabelTenantName)
	}

	aitenantName := annotationValue(tenant, AnnotationAITenantName)
	if aitenantName == "" {
		return PlatformContext{}, fmt.Errorf("AITenant-managed tenant config %s/%s is missing %s", tenant.GetNamespace(), tenant.GetName(), AnnotationAITenantName)
	}
	aitenantNamespace := annotationValue(tenant, AnnotationAITenantNamespace)
	if aitenantNamespace == "" {
		return PlatformContext{}, fmt.Errorf("AITenant-managed tenant config %s/%s is missing %s", tenant.GetNamespace(), tenant.GetName(), AnnotationAITenantNamespace)
	}

	var aitenant maasv1alpha1.AITenant
	key := client.ObjectKey{Name: aitenantName, Namespace: aitenantNamespace}
	if err := c.Get(ctx, key, &aitenant); err != nil {
		return PlatformContext{}, fmt.Errorf("get owning AITenant %s/%s for tenant config %s/%s: %w", key.Namespace, key.Name, tenant.GetNamespace(), tenant.GetName(), err)
	}

	ref := aitenant.Status.GatewayRef
	if ref.Name == "" || ref.Namespace == "" {
		return PlatformContext{}, fmt.Errorf("AITenant %s/%s status.gatewayRef is not ready", aitenant.Namespace, aitenant.Name)
	}

	return PlatformContext{
		GatewayRef:   ref,
		ExternalOIDC: aitenant.Spec.OIDC.DeepCopy(),
		Source:       "aitenant",
	}, nil
}

func isAITenantManagedTenantConfig(tenant client.Object) bool {
	labels := tenant.GetLabels()
	return labels != nil && labels[LabelManagedByAITenant] == "true"
}

// TenantUsesAITenantPlatformContext reports whether gateway/OIDC platform
// context should be read from the owning AITenant rather than legacy Tenant.spec.
func TenantUsesAITenantPlatformContext(tenant client.Object) bool {
	return isAITenantManagedTenantConfig(tenant)
}

// TenantNamespaceForAITenant returns the tenant admin namespace derived from an
// AITenant name and the configured default tenant namespace.
func TenantNamespaceForAITenant(name, defaultTenantNamespace string) string {
	if name == DefaultAITenantName || (defaultTenantNamespace != "" && name == defaultTenantNamespace) {
		if defaultTenantNamespace != "" {
			return defaultTenantNamespace
		}
		return DefaultAITenantName
	}
	return tenantNamespacePrefix + name
}

func annotationValue(obj client.Object, key string) string {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return ""
	}
	return annotations[key]
}
