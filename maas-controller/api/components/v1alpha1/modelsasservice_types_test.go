package v1alpha1

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestModelsAsServiceRegistersExpectedGVK(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add components api to scheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&ModelsAsService{})
	if err != nil {
		t.Fatalf("resolve GVK for ModelsAsService: %v", err)
	}
	if len(gvks) != 1 {
		t.Fatalf("registered GVK count = %d, want 1", len(gvks))
	}

	got := gvks[0]
	if got.Group != GroupVersion.Group {
		t.Fatalf("GVK group = %q, want %q", got.Group, GroupVersion.Group)
	}
	if got.Version != GroupVersion.Version {
		t.Fatalf("GVK version = %q, want %q", got.Version, GroupVersion.Version)
	}
	if got.Kind != ModelsAsServiceKind {
		t.Fatalf("GVK kind = %q, want %q", got.Kind, ModelsAsServiceKind)
	}
}

func TestGeneratedModelsAsServiceCRDContract(t *testing.T) {
	crd := readGeneratedModelsAsServiceCRD(t)

	if crd.Spec.Group != GroupVersion.Group {
		t.Fatalf("CRD group = %q, want %q", crd.Spec.Group, GroupVersion.Group)
	}
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatalf("CRD scope = %q, want %q", crd.Spec.Scope, apiextensionsv1.ClusterScoped)
	}
	if crd.Spec.Names.Kind != ModelsAsServiceKind {
		t.Fatalf("CRD kind = %q, want %q", crd.Spec.Names.Kind, ModelsAsServiceKind)
	}

	version := findVersion(t, crd, GroupVersion.Version)
	schema := version.Schema.OpenAPIV3Schema
	if schema == nil {
		t.Fatal("CRD version schema is nil")
	}

	if !hasValidationRule(schema, "self.metadata.name == 'default-modelsasservice'") {
		t.Fatal("CRD is missing singleton name validation for default-modelsasservice")
	}

	specSchema, ok := schema.Properties["spec"]
	if !ok {
		t.Fatal("CRD schema is missing spec")
	}
	if _, ok := specSchema.Properties["managementState"]; !ok {
		t.Fatal("CRD spec is missing managementState")
	}

	statusSchema, ok := schema.Properties["status"]
	if !ok {
		t.Fatal("CRD schema is missing status")
	}
	for _, field := range []string{"phase", "observedGeneration", "conditions", "releases"} {
		if _, ok := statusSchema.Properties[field]; !ok {
			t.Fatalf("CRD status is missing %q", field)
		}
	}
}

func readGeneratedModelsAsServiceCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	_, thisFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}

	crdPath := filepath.Join(
		filepath.Dir(thisFile),
		"..", "..", "..", "..",
		"deployment", "base", "maas-controller", "crd", "bases",
		"components.platform.opendatahub.io_modelsasservices.yaml",
	)

	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read generated CRD %q: %v", crdPath, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}

	return &crd
}

func findVersion(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, name string) apiextensionsv1.CustomResourceDefinitionVersion {
	t.Helper()

	for _, version := range crd.Spec.Versions {
		if version.Name == name {
			return version
		}
	}

	t.Fatalf("CRD version %q not found", name)
	return apiextensionsv1.CustomResourceDefinitionVersion{}
}

func hasValidationRule(schema *apiextensionsv1.JSONSchemaProps, rule string) bool {
	for _, validation := range schema.XValidations {
		if validation.Rule == rule {
			return true
		}
	}

	return false
}
