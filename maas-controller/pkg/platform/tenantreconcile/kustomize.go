package tenantreconcile

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// overlayDefaultNamespace is the namespace hardcoded in the overlay's
// kustomization.yaml (namespace: opendatahub). postBuildTransform remaps
// it to the actual appNamespace from the Tenant CR.
const overlayDefaultNamespace = "opendatahub"

// RenderKustomize runs kustomize build for the ODH maas-api overlay and
// applies ODH-equivalent namespace remapping and component labels.
func RenderKustomize(manifestDir, appNamespace string) ([]unstructured.Unstructured, error) {
	kustomizationPath := manifestDir
	if !fileExists(filepath.Join(manifestDir, "kustomization.yaml")) {
		kustomizationPath = filepath.Join(manifestDir, "default")
	}

	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	fs := filesys.MakeFsOnDisk()
	resMap, err := k.Run(fs, kustomizationPath)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %q: %w", kustomizationPath, err)
	}

	if err := postBuildTransform(resMap, appNamespace); err != nil {
		return nil, fmt.Errorf("post-build transform: %w", err)
	}

	rendered := resMap.Resources()
	out := make([]unstructured.Unstructured, 0, len(rendered))
	for i := range rendered {
		m, err := rendered[i].Map()
		if err != nil {
			return nil, fmt.Errorf("resource map: %w", err)
		}
		normalizeJSONTypes(m)
		out = append(out, unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// postBuildTransform remaps the overlay's hardcoded default namespace to the
// actual appNamespace and merges ODH component labels into metadata. Unlike the
// blanket kustomize NamespaceTransformerPlugin + LabelTransformerPlugin, this:
//   - Leaves cluster-scoped resources (no namespace) untouched
//   - Preserves cross-namespace resources placed in a non-default namespace by
//     kustomize replacements (e.g., payload-processing in the gateway namespace)
//   - Preserves ClusterRoleBinding/RoleBinding subjects with non-default namespaces
//   - Merges labels into metadata only (not into Deployment selectors, which are
//     already correct from each base's own kustomization)
func postBuildTransform(resMap resmap.ResMap, appNamespace string) error {
	componentLabels := map[string]string{
		LabelODHAppPrefix + "/" + ComponentName: "true",
		LabelK8sPartOf:                          "models-as-a-service",
	}

	for _, res := range resMap.Resources() {
		// --- namespace remapping (uses RNode API, persists directly) ---
		if appNamespace != "" {
			ns := res.GetNamespace()
			if ns == overlayDefaultNamespace {
				if err := res.SetNamespace(appNamespace); err != nil {
					return fmt.Errorf("set namespace on %s %s: %w", res.GetKind(), res.GetName(), err)
				}
			}

			if err := remapSubjectNamespaces(res, appNamespace); err != nil {
				return fmt.Errorf("remap subjects on %s %s: %w", res.GetKind(), res.GetName(), err)
			}
		}

		// --- ODH component labels (uses RNode API, persists directly) ---
		labels := res.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		for k, v := range componentLabels {
			labels[k] = v
		}
		if err := res.SetLabels(labels); err != nil {
			return fmt.Errorf("set labels on %s %s: %w", res.GetKind(), res.GetName(), err)
		}
	}
	return nil
}

// remapSubjectNamespaces rewrites ClusterRoleBinding/RoleBinding subjects that
// reference the overlay default namespace to use appNamespace instead. Uses the
// RNode Pipe API to mutate the underlying YAML tree directly (res.Map() returns
// a detached copy that would discard mutations).
func remapSubjectNamespaces(res *resource.Resource, appNamespace string) error {
	kind := res.GetKind()
	if kind != "ClusterRoleBinding" && kind != "RoleBinding" {
		return nil
	}

	m, err := res.Map()
	if err != nil {
		return fmt.Errorf("map: %w", err)
	}
	subjects, ok := m["subjects"].([]any)
	if !ok {
		return nil
	}

	changed := false
	for _, s := range subjects {
		subj, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sns, ok := subj["namespace"].(string); ok && sns == overlayDefaultNamespace {
			subj["namespace"] = appNamespace
			changed = true
		}
	}
	if !changed {
		return nil
	}

	// Write modified map back to the RNode via YAML round-trip.
	m["subjects"] = subjects
	b, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	node, err := kyaml.Parse(string(b))
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	res.ResetRNode((&resource.Resource{RNode: *node}))
	return nil
}

// normalizeJSONTypes converts Go int values to int64 in an unstructured map.
// Kustomize's resMap.Map() returns int for YAML integers, but
// k8s.io/apimachinery DeepCopyJSONValue only handles int64/float64.
func normalizeJSONTypes(obj map[string]any) {
	for k, v := range obj {
		obj[k] = normalizeValue(v)
	}
}

func normalizeValue(v any) any {
	switch val := v.(type) {
	case int:
		return int64(val)
	case map[string]any:
		normalizeJSONTypes(val)
		return val
	case []any:
		for i, item := range val {
			val[i] = normalizeValue(item)
		}
		return val
	default:
		return v
	}
}

func fileExists(p string) bool {
	fs := filesys.MakeFsOnDisk()
	return fs.Exists(p)
}

// DefaultManifestPath returns MAAS_PLATFORM_MANIFESTS or a dev default relative to cwd (models-as-a-service repo layout).
func DefaultManifestPath() string {
	if v := os.Getenv("MAAS_PLATFORM_MANIFESTS"); v != "" {
		return v
	}
	return "../maas-api/deploy/overlays/odh"
}

// EnsureTenantGatewayDefaults applies the same default gateway ref as ODH when unset.
func EnsureTenantGatewayDefaults(t *maasv1alpha1.Tenant) {
	if t.Spec.GatewayRef.Namespace == "" && t.Spec.GatewayRef.Name == "" {
		t.Spec.GatewayRef.Namespace = DefaultGatewayNamespace
		t.Spec.GatewayRef.Name = DefaultGatewayName
	}
}
