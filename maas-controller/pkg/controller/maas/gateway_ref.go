package maas

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/gatewayresolve"
)

// resolveGatewayRef resolves the gateway reference for a MaaSModelRef.
// When spec.tenantRef is set, it looks up the named AITenant in the AITenant
// infrastructure namespace and uses its Status.GatewayRef directly.
// When spec.tenantRef is empty, it falls back to namespace-based resolution.
func (r *MaaSModelRefReconciler) resolveGatewayRef(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) (maasv1alpha1.TenantGatewayRef, error) {
	if model.Spec.TenantRef != "" {
		aitenant := &maasv1alpha1.AITenant{}
		key := client.ObjectKey{
			Name:      model.Spec.TenantRef,
			Namespace: r.AITenantNamespace,
		}
		if err := r.Get(ctx, key, aitenant); err != nil {
			if apierrors.IsNotFound(err) {
				return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant %q not found in namespace %s", model.Spec.TenantRef, r.AITenantNamespace)
			}
			return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("failed to get AITenant %q: %w", model.Spec.TenantRef, err)
		}
		model.Status.ResolvedTenantRef = model.Spec.TenantRef
		ref := aitenant.Status.GatewayRef
		if ref.Name == "" || ref.Namespace == "" {
			return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("AITenant %q has no gateway reference in status", model.Spec.TenantRef)
		}
		log.V(4).Info("Resolved gateway from AITenant", "aiTenant", model.Spec.TenantRef, "gateway", fmt.Sprintf("%s/%s", ref.Namespace, ref.Name))
		return ref, nil
	}

	model.Status.ResolvedTenantRef = ""
	return gatewayresolve.ForNamespace(
		ctx,
		r.Client,
		model.Namespace,
		r.AITenantNamespace,
		r.DefaultTenantNamespace,
		r.gatewayName(),
		r.gatewayNamespace(),
		r.TenantNamespaceDiscoveryEnabled,
	)
}
