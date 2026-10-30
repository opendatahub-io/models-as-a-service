package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// InferenceCRDs reads the two inference schemas installed by MaaS. They are
// generated in ai-gateway-controller and synchronized into the deployment bundle.
func InferenceCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, plural := range []string{"externalmodels", "externalproviders"} {
		path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-controller", "crd", "bases", "inference.opendatahub.io_"+plural+".yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(data, crd); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		crds = append(crds, crd)
	}
	return crds, nil
}

// InferenceSchemaContract is the validation schema AGC generates under
// config/crd/bases. Descriptions are omitted so documentation changes do not
// obscure API drift. Regenerate it when synchronizing the CRDs; the command is in
// ai-gateway-payload-processing's api/inference/v1alpha1/testdata/README.md.
func InferenceSchemaContract() (map[string]apiextensionsv1.JSONSchemaProps, error) {
	return inferenceSchemaFixture("agc-inference-schema.json")
}

// LegacyInferenceCRDs restores the validation baseline installed from MaaS
// commit 353a85e841d8442afc976a539e49e2925b73e017, also used by AGC's schema tests.
// Keep this fixture unchanged to test real upgrades from the old contract.
func LegacyInferenceCRDs() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	crds, err := InferenceCRDs()
	if err != nil {
		return nil, err
	}
	schemas, err := inferenceSchemaFixture("installed-legacy-inference-schema.json")
	if err != nil {
		return nil, err
	}
	for _, crd := range crds {
		schema, ok := schemas[crd.Spec.Names.Plural]
		if !ok {
			return nil, fmt.Errorf("missing legacy schema for %s", crd.Name)
		}
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema = schema.DeepCopy()
	}
	return crds, nil
}

func inferenceSchemaFixture(name string) (map[string]apiextensionsv1.JSONSchemaProps, error) {
	data, err := os.ReadFile(filepath.Join("fixture", "testdata", name))
	if err != nil {
		return nil, err
	}
	var schemas map[string]apiextensionsv1.JSONSchemaProps
	if err := json.Unmarshal(data, &schemas); err != nil {
		return nil, err
	}
	return schemas, nil
}

// InferenceModel creates a legacy-shaped model without explicit attachments.
func InferenceModel(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inference.opendatahub.io/v1alpha1",
		"kind":       "ExternalModel",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{"externalProviderRefs": []any{map[string]any{
			"ref":         map[string]any{"name": "provider"},
			"targetModel": "test-model",
			"apiFormat":   "openai-chat",
			"path":        "/v1/chat/completions",
			"auth":        map[string]any{"type": "simple", "secretRef": map[string]any{"name": "a.-b"}},
		}}},
	}}
}

// InferenceProvider uses auth and reference names accepted by the old schema.
func InferenceProvider(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inference.opendatahub.io/v1alpha1",
		"kind":       "ExternalProvider",
		"metadata":   map[string]any{"name": "provider", "namespace": namespace},
		"spec": map[string]any{
			"provider": "openai",
			"endpoint": "api.example.com",
			"auth":     map[string]any{"type": "simple", "secretRef": map[string]any{"name": "a..b"}},
		},
	}}
}
