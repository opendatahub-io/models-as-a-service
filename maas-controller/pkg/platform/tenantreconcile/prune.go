package tenantreconcile

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PruneLegacyCleanupResources removes ephemeral-key cleanup operands dropped from the
// platform overlay in PR934. ApplyRendered is SSA-only and does not prune resources
// removed from manifests; this runs after apply so upgrades converge automatically.
//
// The cleanup CronJob was renamed per tenant (maas-api-key-cleanup-<tenantID>) while the
// default tenant kept the bare name, so both names are pruned for the given tenant. The
// NetworkPolicy was never suffixed and is always pruned by its bare name.
func PruneLegacyCleanupResources(ctx context.Context, log logr.Logger, c client.Client, appNs, tenantID string) error {
	type legacyResource struct {
		gvk  schema.GroupVersionKind
		kind string
		name string
	}

	legacy := []legacyResource{
		{gvk: GVKNetworkPolicy, kind: "NetworkPolicy", name: LegacyMaaSAPICleanupNetworkPolicyName},
	}
	for _, name := range legacyCleanupCronJobNames(tenantID) {
		legacy = append(legacy, legacyResource{gvk: GVKCronJob, kind: "CronJob", name: name})
	}

	for _, resource := range legacy {
		if err := deleteLegacyResourceIfExists(ctx, log, c, resource.kind, resource.name, appNs, resource.gvk); err != nil {
			return err
		}
	}
	return nil
}

// legacyCleanupCronJobNames returns the legacy cleanup CronJob names to prune for a tenant:
// the tenant-scoped name plus the bare name, de-duplicated (they coincide for the default
// tenant where tenantID is empty).
func legacyCleanupCronJobNames(tenantID string) []string {
	tenantScoped := LegacyMaaSAPIKeyCleanupCronJobNameForTenant(tenantID)
	if tenantScoped == LegacyMaaSAPIKeyCleanupCronJobName {
		return []string{LegacyMaaSAPIKeyCleanupCronJobName}
	}
	return []string{tenantScoped, LegacyMaaSAPIKeyCleanupCronJobName}
}

func deleteLegacyResourceIfExists(ctx context.Context, log logr.Logger, c client.Client, kind, name, namespace string, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get legacy %s/%s in namespace %s: %w", kind, name, namespace, err)
	}
	if !isManagedForPrune(obj) {
		log.V(1).Info("Skipping legacy resource prune: annotation "+AnnotationManaged+":false",
			"kind", kind, "name", name, "namespace", namespace)
		return nil
	}
	log.Info("Deleting legacy platform resource", "kind", kind, "name", name, "namespace", namespace)
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete legacy %s/%s in namespace %s: %w", kind, name, namespace, err)
	}
	return nil
}

func isManagedForPrune(obj metav1.Object) bool {
	ann := obj.GetAnnotations()
	if ann != nil && ann[AnnotationManaged] == "false" {
		return false
	}
	return true
}
