package maas

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// TeardownRequestedAnnotation on the maas-controller Deployment tells LifecycleReconciler
// to start teardown: bootstrap/self-heal behaviors stay disabled, AITenant resources are
// deleted, then Config/default is deleted as a plain step, and finally
// TeardownCompletedAnnotation is set once nothing is pending. Namespaces are preserved.
const TeardownRequestedAnnotation = "maas.opendatahub.io/teardown-requested"

// TeardownCompletedAnnotation is set on the maas-controller Deployment by LifecycleReconciler
// once every ordered teardown step, including Config/default deletion, has finished.
// External operators watch or poll this annotation as the "self-teardown is done" signal
// instead of depending on Config (or anything cascade-deleted through it) to still exist.
const TeardownCompletedAnnotation = "maas.opendatahub.io/teardown-completed"

const teardownRequeueAfter = 5 * time.Second

// TeardownRequestedOnDeployment reports whether the maas-controller Deployment has been
// annotated to start teardown. Callers outside this file (e.g. the Reconcile entrypoint,
// the default AITenant bootstrap gate, and the AITenant namespace monitor in cmd/manager)
// use this as the single source of truth for "is teardown in progress".
func TeardownRequestedOnDeployment(dep *appsv1.Deployment) bool {
	if dep == nil {
		return false
	}
	return dep.GetAnnotations()[TeardownRequestedAnnotation] == "true"
}

var resourceTypesToRemove = []schema.GroupVersionKind{
	{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "AITenant"},
}

// handleRequestedTeardown runs on every reconcile while TeardownRequestedAnnotation is
// present on the maas-controller Deployment. It deletes AITenant resources, requeueing
// until nothing is pending; once clean, it deletes Config/default and then marks
// TeardownCompletedAnnotation on the Deployment. It does not delete namespaces.
//
// Config deletion is a plain, unguarded step (no finalizer): this reconciler is the only
// actor that deletes Config while teardown is requested, so ordering is enforced here in
// Go, not by the API server. Deleting Config before writing the completion annotation is
// safe because Reconcile strips any legacy Deployment->Config ownerReference on every pass
// (see stripLegacyDeploymentConfigOwnerReference): the Deployment is never a GC dependent
// of Config, so cascade-deleting Config's other dependents (ServiceMonitor, dashboards,
// EnvoyFilter) cannot take the Deployment down with it before the annotation is written.
// If the process crashes between the two steps, the next reconcile finds nothing pending
// and no Config, and simply (idempotently) marks completion.
func (r *LifecycleReconciler) handleRequestedTeardown(ctx context.Context, dep *appsv1.Deployment, cfg *maasv1alpha1.Config) (ctrl.Result, error) {
	pending, err := r.cleanupTeardownResources(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: teardownRequeueAfter}, nil
	}

	if cfg != nil {
		if err := r.Delete(ctx, cfg); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete Config/default during teardown: %w", err)
		}
	}

	if err := r.markTeardownCompleted(ctx, dep); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// markTeardownCompleted sets TeardownCompletedAnnotation on the Deployment so external
// operators have a single durable signal for "self-teardown is done" that survives
// Config (and anything cascade-deleted through it) disappearing. Idempotent so patching
// it does not re-trigger this reconciler indefinitely through the Deployment watch.
func (r *LifecycleReconciler) markTeardownCompleted(ctx context.Context, dep *appsv1.Deployment) error {
	if dep == nil {
		return nil
	}
	if dep.GetAnnotations()[TeardownCompletedAnnotation] == "true" {
		return nil
	}
	base := dep.DeepCopy()
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[TeardownCompletedAnnotation] = "true"
	if err := r.Patch(ctx, dep, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("annotate Deployment with %s: %w", TeardownCompletedAnnotation, err)
	}
	return nil
}

func (r *LifecycleReconciler) cleanupTeardownResources(ctx context.Context) (bool, error) {
	resourcesPending := false
	for _, resourceGVK := range resourceTypesToRemove {
		items, err := listUnstructuredByGVK(ctx, r.Client, resourceGVK)
		if err != nil {
			return false, err
		}
		if len(items) == 0 {
			continue
		}
		resourcesPending = true

		for i := range items {
			obj := items[i].DeepCopy()
			if !obj.GetDeletionTimestamp().IsZero() {
				continue
			}
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return false, fmt.Errorf("delete %s %s/%s during teardown: %w",
					resourceGVK.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
	return resourcesPending, nil
}

func listUnstructuredByGVK(ctx context.Context, cli client.Client, resourceGVK schema.GroupVersionKind) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   resourceGVK.Group,
		Version: resourceGVK.Version,
		Kind:    resourceGVK.Kind + "List",
	})

	if err := cli.List(ctx, list); err != nil {
		if isNoMatchOrNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s during teardown: %w", resourceGVK.String(), err)
	}

	return list.Items, nil
}

func isNoMatchOrNotFound(err error) bool {
	return errors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}
