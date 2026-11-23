package maas

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const staleUsageDatasourceName = "kuadrant-prometheus-datasource"

// cleanupStaleResources removes obsolete resources from previous releases in the
// controller (applications) namespace, independently of module setup. Add future
// stale resources here to share the same lock and completion tracking.
// Cleanup completes once per reconciler lifetime, with retries after errors or
// when optional APIs appear. Resources predating Config ownership are identified
// by reserved names and resource-specific checks; never sweep other namespaces.
func (r *LifecycleReconciler) cleanupStaleResources(ctx context.Context, log logr.Logger) error {
	if r.DeploymentNS == "" {
		return nil
	}

	r.staleResourcesCleanupMu.Lock()
	defer r.staleResourcesCleanupMu.Unlock()
	if r.staleResourcesCleanupCompleted {
		return nil
	}
	missingAPI := false

	for _, resource := range []struct {
		group   string
		version string
		kind    string
		name    string
	}{
		{group: "perses.dev", version: "v1alpha1", kind: "PersesDashboard", name: "dashboard-3-maas-usage-admin"},
		{group: "perses.dev", version: "v1alpha1", kind: "PersesDatasource", name: staleUsageDatasourceName},
		{version: "v1", kind: "ConfigMap", name: "prometheus-web-tls-ca"},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: resource.group, Version: resource.version, Kind: resource.kind})
		key := client.ObjectKey{Namespace: r.DeploymentNS, Name: resource.name}
		if err := r.Get(ctx, key, obj); err != nil {
			if apimeta.IsNoMatchError(err) {
				missingAPI = true
				continue
			}
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get stale %s %s: %w", resource.kind, key, err)
		}

		if resource.kind == "PersesDashboard" {
			// Both namespaces can coincide. Preserve the current opt-in dashboard.
			displayName, _, err := unstructured.NestedString(obj.Object, "spec", "display", "name")
			if err != nil {
				return fmt.Errorf("read display name of %s: %w", key, err)
			}
			if displayName == "Usage (legacy)" {
				continue
			}
		}
		// Do not delete a resource replaced or modified after the identity check.
		uid, version := obj.GetUID(), obj.GetResourceVersion()
		if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete stale %s %s: %w", resource.kind, key, err)
		}
		log.Info("removed stale resource", "kind", resource.kind, "namespace", key.Namespace, "name", key.Name)
	}
	r.staleResourcesCleanupCompleted = !missingAPI
	return nil
}
