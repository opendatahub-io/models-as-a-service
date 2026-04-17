package tenantreconcile

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/api/builtins" //nolint:staticcheck // no replacement until kustomize API v1
	"sigs.k8s.io/kustomize/api/filters/namespace"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// createNamespaceApplierPlugin mirrors opendatahub-operator/pkg/plugins.CreateNamespaceApplierPlugin.
func createNamespaceApplierPlugin(targetNamespace string) *builtins.NamespaceTransformerPlugin {
	return &builtins.NamespaceTransformerPlugin{
		ObjectMeta: types.ObjectMeta{
			Name:      "maas-namespace-plugin",
			Namespace: targetNamespace,
		},
		FieldSpecs: []types.FieldSpec{
			{Gvk: resid.Gvk{}, Path: "metadata/namespace", CreateIfNotPresent: true},
			{Gvk: resid.Gvk{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}, Path: "subjects/namespace", CreateIfNotPresent: true},
			{Gvk: resid.Gvk{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}, Path: "subjects/namespace", CreateIfNotPresent: true},
			{Gvk: resid.Gvk{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}, Path: "webhooks/clientConfig/service/namespace", CreateIfNotPresent: false},
			{Gvk: resid.Gvk{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}, Path: "webhooks/clientConfig/service/namespace", CreateIfNotPresent: false},
			{Gvk: resid.Gvk{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}, Path: "spec/conversion/webhook/clientConfig/service/namespace", CreateIfNotPresent: false},
		},
		UnsetOnly:              false,
		SetRoleBindingSubjects: namespace.AllServiceAccountSubjects,
	}
}

func odhComponentLabels() map[string]string {
	return map[string]string{
		LabelODHAppPrefix + "/" + ComponentName: "true",
		LabelK8sPartOf:                          "models-as-a-service",
	}
}

func createSetLabelsPlugin(labels map[string]string) *builtins.LabelTransformerPlugin {
	return &builtins.LabelTransformerPlugin{
		Labels: labels,
		FieldSpecs: []types.FieldSpec{
			{Gvk: resid.Gvk{Kind: "Deployment"}, Path: "spec/template/metadata/labels", CreateIfNotPresent: true},
			{Gvk: resid.Gvk{Kind: "Deployment"}, Path: "spec/selector/matchLabels", CreateIfNotPresent: true},
			{Gvk: resid.Gvk{}, Path: "metadata/labels", CreateIfNotPresent: true},
		},
	}
}

// RenderKustomize runs kustomize build for the ODH maas-api overlay and applies ODH-equivalent namespace + labels.
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

	if appNamespace != "" {
		plugin := createNamespaceApplierPlugin(appNamespace)
		if err := plugin.Transform(resMap); err != nil {
			return nil, fmt.Errorf("namespace transform: %w", err)
		}
	}

	labelPlugin := createSetLabelsPlugin(odhComponentLabels())
	if err := labelPlugin.Transform(resMap); err != nil {
		return nil, fmt.Errorf("labels transform: %w", err)
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
