package tenantreconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
)

// overlayDefaultNamespace is the namespace hardcoded in the overlay's
// kustomization.yaml (namespace: opendatahub). postBuildTransform remaps
// it to the actual appNamespace for the tenant config.
const overlayDefaultNamespace = "opendatahub"

// RenderKustomize runs kustomize build for the ODH maas-api overlay and
// applies ODH-equivalent namespace remapping and component labels.
func RenderKustomize(manifestDir, appNamespace string) ([]unstructured.Unstructured, error) {
	kustomizationPath, err := resolveKustomizationPath(manifestDir)
	if err != nil {
		return nil, err
	}

	buildPath := kustomizationPath
	cleanup := func() {}
	if supportsMetricsParamsPatch(kustomizationPath) && appNamespace != "" && appNamespace != overlayDefaultNamespace {
		buildPath, cleanup, err = withBuildNamespace(kustomizationPath, appNamespace)
		if err != nil {
			return nil, err
		}
	}
	defer cleanup()

	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	fs := filesys.MakeFsOnDisk()
	resMap, err := k.Run(fs, buildPath)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %q: %w", buildPath, err)
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

func resolveKustomizationPath(manifestDir string) (string, error) {
	if fileExists(filepath.Join(manifestDir, "kustomization.yaml")) {
		return manifestDir, nil
	}
	fallback := filepath.Join(manifestDir, "default")
	if fileExists(filepath.Join(fallback, "kustomization.yaml")) {
		return fallback, nil
	}
	return "", fmt.Errorf("kustomization.yaml not found under %q", manifestDir)
}

// supportsMetricsParamsPatch reports whether the overlay includes maas-api secure
// metrics and the maas-parameters ConfigMap used for ServiceMonitor serverName.
func supportsMetricsParamsPatch(kustomizationPath string) bool {
	return strings.HasSuffix(filepath.ToSlash(kustomizationPath), "maas-api/deploy/overlays/odh")
}

// withBuildNamespace returns a kustomize root that patches maas-parameters.data.namespace
// and ServiceMonitor tlsConfig.serverName before build when appNamespace differs from
// the overlay default. This mirrors the AGO path (#1401).
func withBuildNamespace(kustomizationPath, appNamespace string) (string, func(), error) {
	noop := func() {}

	tmpDir, err := os.MkdirTemp("", "maas-kustomize-*")
	if err != nil {
		return "", noop, fmt.Errorf("create temp kustomize dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	absPath, err := filepath.Abs(kustomizationPath)
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("resolve kustomization path: %w", err)
	}

	overlayLink := filepath.Join(tmpDir, "overlay")
	if err := os.Symlink(absPath, overlayLink); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("symlink overlay: %w", err)
	}

	kust := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - overlay
patches:
  - target:
      kind: ConfigMap
      name: maas-parameters
    patch: |
      - op: replace
        path: /data/namespace
        value: %q
  - target:
      kind: ServiceMonitor
      name: maas-api-metrics
    patch: |
      - op: replace
        path: /spec/endpoints/0/tlsConfig/serverName
        value: "maas-api-metrics.%s.svc"
`, appNamespace, appNamespace)

	if err := os.WriteFile(filepath.Join(tmpDir, "kustomization.yaml"), []byte(kust), 0o600); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("write temp kustomization: %w", err)
	}

	return tmpDir, cleanup, nil
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

// ManifestPathForPlatform returns the appropriate kustomize overlay path based on
// whether the cluster is OpenShift (isOCP=true) or vanilla Kubernetes (isOCP=false).
// The xKS overlay avoids OCP-specific resources like service-serving-certs and
// service-ca ConfigMap injection that don't exist on non-OpenShift clusters.
func ManifestPathForPlatform(isOCP bool) string {
	if v := os.Getenv("MAAS_PLATFORM_MANIFESTS"); v != "" {
		return v
	}
	if isOCP {
		return "/maas-api/deploy/overlays/odh"
	}
	return "/maas-api/deploy/overlays/xks"
}
