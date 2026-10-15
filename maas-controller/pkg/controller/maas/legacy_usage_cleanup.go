package maas

import (
	"context"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const legacyUsageDatasourceName = "kuadrant-prometheus-datasource"

// cleanupLegacyUsageResources migrates the 3.4 metrics dashboard resources in the
// controller (applications) namespace. They predate Config ownership, so the
// normal owner-based cleanup cannot identify them. Match their reserved names
// and legacy datasource configuration instead; never sweep other namespaces.
func (r *LifecycleReconciler) cleanupLegacyUsageResources(ctx context.Context, log logr.Logger) error {
	if r.DeploymentNS == "" {
		return nil
	}

	for _, resource := range []struct {
		kind string
		name string
	}{
		{kind: "PersesDashboard", name: "dashboard-3-maas-usage-admin"},
		{kind: "PersesDatasource", name: legacyUsageDatasourceName},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: resource.kind})
		key := client.ObjectKey{Namespace: r.DeploymentNS, Name: resource.name}
		if err := r.Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("get legacy %s %s: %w", resource.kind, key, err)
		}

		if !isLegacyUsageResource(obj) {
			continue
		}
		// Do not delete a resource replaced or modified after the identity check.
		uid, version := obj.GetUID(), obj.GetResourceVersion()
		if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete legacy %s %s: %w", resource.kind, key, err)
		}
		log.Info("removed legacy 3.4 metrics usage resource", "kind", resource.kind, "namespace", key.Namespace, "name", key.Name)
	}
	return nil
}

func isLegacyUsageResource(obj *unstructured.Unstructured) bool {
	if obj.GetKind() == "PersesDashboard" {
		// The current opt-in dashboard has the same name but queries
		// data-science-prometheus-datasource, including when both namespaces coincide.
		return referencesLegacyUsageDatasource(obj.Object["spec"])
	}
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "config", "plugin", "kind")
	url, _, _ := unstructured.NestedString(obj.Object, "spec", "config", "plugin", "spec", "proxy", "spec", "url")
	return kind == "PrometheusDatasource" && url == "https://thanos-querier.openshift-monitoring.svc:9092?namespace=kuadrant-system"
}

func referencesLegacyUsageDatasource(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		if datasource, ok := node["datasource"].(map[string]any); ok &&
			datasource["kind"] == "PrometheusDatasource" && datasource["name"] == legacyUsageDatasourceName {
			return true
		}
		for _, child := range node {
			if referencesLegacyUsageDatasource(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(node, referencesLegacyUsageDatasource)
	}
	return false
}
