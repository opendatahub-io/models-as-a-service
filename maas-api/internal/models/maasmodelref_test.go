package models //nolint:testpackage // tests access unexported maasModelRefToModel and kind constants

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	testDefaultMetadataName = "my-model"
	testExternalModelName   = "gpt-4o-external"
	testExternalRefName     = "ext-ref"
)

func TestMaasModelRefToModel_NilInput(t *testing.T) {
	m := maasModelRefToModel(nil)
	assert.Nil(t, m)
}

func TestMaasModelRefToModel_ModelID(t *testing.T) {
	tests := []struct {
		name          string
		metadataName  string
		modelRefName  string
		resolvedAlias string
		expectedID    string
		expectedKind  string
		modelRefKind  string
	}{
		{
			name:          "LLMInferenceService with resolvedModelAlias uses spec.modelRef.name",
			metadataName:  testDefaultMetadataName,
			modelRefName:  "qwen3-8b-fp8-dynamic",
			resolvedAlias: "publishers/ai-eng-cracow/models/qwen3-8b-fp8-dynamic",
			expectedID:    "qwen3-8b-fp8-dynamic",
			expectedKind:  kindLLMISvcAlternate,
			modelRefKind:  kindLLMISvcAlternate,
		},
		{
			name:          "LLMInferenceService without resolvedModelAlias uses spec.modelRef.name",
			metadataName:  testDefaultMetadataName,
			modelRefName:  "llama-7b",
			resolvedAlias: "",
			expectedID:    "llama-7b",
			expectedKind:  kindLLMISvcAlternate,
			modelRefKind:  kindLLMISvcAlternate,
		},
		{
			name:          "LLMInferenceService without modelRef.name falls back to metadata.name",
			metadataName:  testDefaultMetadataName,
			modelRefName:  "",
			resolvedAlias: "publishers/ns/models/some-model",
			expectedID:    testDefaultMetadataName,
			expectedKind:  kindLLMISvcAlternate,
			modelRefKind:  kindLLMISvcAlternate,
		},
		{
			name:         "empty kind defaults to llmisvc",
			metadataName: testDefaultMetadataName,
			modelRefName: "bert-base",
			expectedID:   "bert-base",
			expectedKind: kindLLMISvc,
			modelRefKind: "",
		},
		{
			name:          "ExternalModel uses spec.modelRef.name",
			metadataName:  testExternalRefName,
			modelRefName:  testExternalModelName,
			resolvedAlias: testExternalModelName,
			expectedID:    testExternalModelName,
			expectedKind:  kindExternalModel,
			modelRefKind:  kindExternalModel,
		},
		{
			name:         "ExternalModel without modelRef.name falls back to metadata.name",
			metadataName: testExternalRefName,
			modelRefName: "",
			expectedID:   testExternalRefName,
			expectedKind: kindExternalModel,
			modelRefKind: kindExternalModel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "maas.opendatahub.io/v1alpha1",
				"kind":       "MaaSModelRef",
				"metadata": map[string]any{
					"name":              tc.metadataName,
					"namespace":         "test-ns",
					"creationTimestamp": time.Now().Format("2006-01-02T15:04:05Z"),
				},
				"spec": map[string]any{
					"modelRef": map[string]any{
						"kind": tc.modelRefKind,
						"name": tc.modelRefName,
					},
				},
				"status": map[string]any{
					"phase":    "Ready",
					"endpoint": "https://example.com",
				},
			}}

			if tc.resolvedAlias != "" {
				_ = unstructured.SetNestedField(u.Object, tc.resolvedAlias, "status", "resolvedModelAlias")
			}

			m := maasModelRefToModel(u)
			require.NotNil(t, m)
			assert.Equal(t, tc.expectedID, m.ID)
			assert.Equal(t, tc.expectedKind, m.Kind)
		})
	}
}
