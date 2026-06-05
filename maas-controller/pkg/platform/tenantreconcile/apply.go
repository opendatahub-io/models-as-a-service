package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const ssaFieldOwner = "maas-controller"

// ApplyRendered server-side-applies rendered objects with Config as controller owner.
//
// The cluster-scoped Config is a valid owner for namespaced resources in any namespace
// and for cluster-scoped operands. Tenant tracking labels are always applied so the Tenant
// reconciler can correlate resources with the subscription-namespace Tenant CR for status and debugging.
//
// Objects matched by skipConfigControllerOwnerRef (see configOwnerRefSkips) do not receive a
// Config controller ownerReference; they still receive tenant tracking labels. Add predicates
// there for future exceptions (e.g. shared config that must outlive Config GC).
func ApplyRendered(ctx context.Context, c client.Client, scheme *runtime.Scheme, tenant *maasv1alpha1.Tenant, appNs string, mcfg *maasv1alpha1.Config, objs []unstructured.Unstructured) error {
	if mcfg == nil || mcfg.UID == "" {
		return errors.New("config with UID is required for platform apply")
	}

	for i := range objs {
		u := objs[i].DeepCopy()

		// Skip resources whose live cluster copy has opendatahub.io/managed=false,
		// allowing operators to opt specific resources out of reconciliation.
		if isLiveResourceUnmanaged(ctx, c, u) {
			ctrl.LoggerFrom(ctx).V(1).Info("Skipping SSA for resource with opendatahub.io/managed=false on cluster",
				"kind", u.GetKind(), "name", u.GetName(), "namespace", u.GetNamespace())
			continue
		}

		// Skip resources whose live cluster copy is already owned by a different
		// controller (e.g. ODH operator's ModelsAsService component). SSA-applying
		// over them would fail on immutable fields like spec.selector and produce
		// conflicting controller:true ownerReferences.
		if isOwnedByExternalController(ctx, c, u, mcfg.UID) {
			ctrl.LoggerFrom(ctx).Info("Skipping SSA: resource owned by external controller",
				"kind", u.GetKind(), "namespace", u.GetNamespace(), "name", u.GetName())
			continue
		}

		if skipConfigControllerOwnerRef(u, appNs) {
			setTenantTrackingLabels(u, tenant)
		} else {
			if err := controllerutil.SetControllerReference(mcfg, u, scheme); err != nil {
				var already *controllerutil.AlreadyOwnedError
				if errors.As(err, &already) {
					ctrl.LoggerFrom(ctx).Info("skipping Config controller reference: object already owned by another controller",
						"kind", u.GetKind(), "namespace", u.GetNamespace(), "name", u.GetName(),
						"existingOwner", already)
				} else {
					return fmt.Errorf("set controller reference (Config) on %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
				}
			}
			setTenantTrackingLabels(u, tenant)
		}
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(u.Object, "status")
		// ForceOwnership is intentional: maas-controller is the sole manager for
		// Tenant platform resources, ensuring a clean field-manager handoff.
		if err := c.Patch(ctx, u, client.Apply, client.FieldOwner(ssaFieldOwner), client.ForceOwnership); err != nil {
			if apimeta.IsNoMatchError(err) && isOptionalAPIGroup(u.GroupVersionKind().Group) {
				// CRD not yet registered for a known optional dependency (e.g. Perses CRDs
				// installed by COO which may not be present yet). Skip so the rest of the
				// platform manifests are applied and Tenant reconcile does not fail.
				// The CRD watch will re-trigger reconcile once the CRDs appear.
				ctrl.LoggerFrom(ctx).Info("skipping resource: optional CRD not yet registered, will apply once installed",
					"group", u.GroupVersionKind().Group, "kind", u.GetKind(),
					"name", u.GetName(), "namespace", u.GetNamespace())
				continue
			}
			return fmt.Errorf("apply %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	return nil
}

// configOwnerRefSkip matches rendered objects that must not get Config as controller owner
// (invalid self-reference, or operands that should not cascade-delete with Config).
type configOwnerRefSkip func(u *unstructured.Unstructured, appNs string) bool

// configOwnerRefSkips is evaluated in order; add new predicates here for additional exceptions.
var configOwnerRefSkips = []configOwnerRefSkip{
	isMaaSControllerDeployment,
}

func skipConfigControllerOwnerRef(u *unstructured.Unstructured, appNs string) bool {
	for _, fn := range configOwnerRefSkips {
		if fn(u, appNs) {
			return true
		}
	}
	return false
}

func isMaaSControllerDeployment(u *unstructured.Unstructured, appNs string) bool {
	if appNs == "" || u.GetNamespace() != appNs {
		return false
	}
	return strings.EqualFold(u.GetKind(), "Deployment") && u.GetName() == MaaSControllerDeploymentName
}

func isLiveResourceUnmanaged(ctx context.Context, c client.Client, rendered *unstructured.Unstructured) bool {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(rendered.GroupVersionKind())
	key := client.ObjectKeyFromObject(rendered)
	if key.Name == "" {
		return false
	}
	if err := c.Get(ctx, key, live); err != nil {
		return false
	}
	ann := live.GetAnnotations()
	return ann != nil && ann[AnnotationManaged] == "false"
}

// isOwnedByExternalController returns true when the live cluster copy of the
// rendered resource has a controller:true ownerReference whose UID differs from
// the given Config UID. This prevents the Tenant reconciler from SSA-applying
// over resources the ODH operator's ModelsAsService component already owns,
// which would fail on immutable fields (spec.selector) and produce conflicting
// controller ownerReferences.
func isOwnedByExternalController(ctx context.Context, c client.Client, rendered *unstructured.Unstructured, configUID types.UID) bool {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(rendered.GroupVersionKind())
	key := client.ObjectKeyFromObject(rendered)
	if key.Name == "" {
		return false
	}
	if err := c.Get(ctx, key, live); err != nil {
		return false
	}
	for _, ref := range live.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID != configUID {
			return true
		}
	}
	return false
}

func setTenantTrackingLabels(obj *unstructured.Unstructured, tenant *maasv1alpha1.Tenant) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[LabelTenantName] = tenant.Name
	labels[LabelTenantNamespace] = tenant.Namespace
	obj.SetLabels(labels)
}
