package tenantreconcile

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	// AnnotationAITenantName identifies the AITenant that owns an AITenant-managed
	// bridge Tenant/default-tenant object.
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

// ResolvePlatformContext resolves the gateway and OIDC values for a Tenant.
//
// AITenant-managed Tenants use their owning AITenant as the source of platform
// context. Legacy/unmanaged Tenants preserve the previous behavior and use
// Tenant.spec values, falling back to fallbackGatewayRef when gatewayRef is
// omitted.
func ResolvePlatformContext(ctx context.Context, c client.Reader, tenant *maasv1alpha1.Tenant, fallbackGatewayRef maasv1alpha1.TenantGatewayRef) (PlatformContext, error) {
	if tenant == nil {
		return PlatformContext{GatewayRef: fallbackGatewayRef, Source: "default"}, nil
	}

	if isAITenantManagedTenant(tenant) {
		return resolveAITenantPlatformContext(ctx, c, tenant)
	}

	ref := tenant.Spec.GatewayRef
	switch {
	case ref.Name == "" && ref.Namespace == "":
		ref = fallbackGatewayRef
	case ref.Name == "" || ref.Namespace == "":
		return PlatformContext{}, fmt.Errorf("tenant %s/%s spec.gatewayRef must set both name and namespace", tenant.Namespace, tenant.Name)
	}

	return PlatformContext{
		GatewayRef:   ref,
		ExternalOIDC: tenant.Spec.ExternalOIDC.DeepCopy(),
		Source:       "tenant-spec",
	}, nil
}

func resolveAITenantPlatformContext(ctx context.Context, c client.Reader, tenant *maasv1alpha1.Tenant) (PlatformContext, error) {
	tenantName := tenant.GetLabels()[LabelTenantName]
	if tenantName == "" {
		return PlatformContext{}, fmt.Errorf("AITenant-managed tenant %s/%s is missing %s", tenant.Namespace, tenant.Name, LabelTenantName)
	}

	aitenantName := annotationValue(tenant, AnnotationAITenantName)
	if aitenantName == "" {
		aitenantName = tenantName
	}
	aitenantNamespace := annotationValue(tenant, AnnotationAITenantNamespace)
	if aitenantNamespace == "" {
		return PlatformContext{}, fmt.Errorf("AITenant-managed tenant %s/%s is missing %s", tenant.Namespace, tenant.Name, AnnotationAITenantNamespace)
	}

	var aitenant maasv1alpha1.AITenant
	key := client.ObjectKey{Name: aitenantName, Namespace: aitenantNamespace}
	if err := c.Get(ctx, key, &aitenant); err != nil {
		return PlatformContext{}, fmt.Errorf("get owning AITenant %s/%s for tenant %s/%s: %w", key.Namespace, key.Name, tenant.Namespace, tenant.Name, err)
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

func isAITenantManagedTenant(tenant *maasv1alpha1.Tenant) bool {
	labels := tenant.GetLabels()
	return labels != nil && labels[LabelManagedByAITenant] == "true"
}

// TenantUsesAITenantPlatformContext reports whether gateway/OIDC platform
// context should be read from the owning AITenant rather than Tenant.spec.
func TenantUsesAITenantPlatformContext(tenant *maasv1alpha1.Tenant) bool {
	return isAITenantManagedTenant(tenant)
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
