/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package weight

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGetExternalProviderRefs(t *testing.T) {
	tests := []struct {
		name        string
		obj         *unstructured.Unstructured
		wantRefs    []providerRef
		wantErr     bool
		errContains string
	}{
		{
			name: "multiple providers with weights",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-a"},
						"targetModel": "model-a",
						"weight":      int64(70),
					},
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-b"},
						"targetModel": "model-b",
						"weight":      int64(30),
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "model-a", Weight: 70},
				{RefName: "provider-b", TargetModel: "model-b", Weight: 30},
			},
			wantErr: false,
		},
		{
			name: "weight as float64 (JSON unmarshaling)",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-a"},
						"targetModel": "model-a",
						"weight":      float64(50),
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "model-a", Weight: 50},
			},
			wantErr: false,
		},
		{
			name: "missing weight defaults to 1",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-a"},
						"targetModel": "model-a",
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "model-a", Weight: 1},
			},
			wantErr: false,
		},
		{
			name: "single provider",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-a"},
						"targetModel": "model-a",
						"weight":      int64(100),
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "model-a", Weight: 100},
			},
			wantErr: false,
		},
		{
			name: "empty externalProviderRefs",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{},
			}),
			wantRefs: []providerRef{},
			wantErr:  false,
		},
		{
			name: "no externalProviderRefs field",
			obj: newExternalModel(map[string]interface{}{
				"someOtherField": "value",
			}),
			wantRefs: nil,
			wantErr:  false,
		},
		{
			name: "missing spec",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "inference.opendatahub.io/v1alpha1",
					"kind":       "ExternalModel",
					"metadata": map[string]interface{}{
						"name":      "test-model",
						"namespace": "test-ns",
					},
				},
			},
			wantRefs:    nil,
			wantErr:     true,
			errContains: "spec not found",
		},
		{
			name: "partial provider data",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					map[string]interface{}{
						"ref": map[string]interface{}{"name": "provider-a"},
					},
					map[string]interface{}{
						"targetModel": "model-b",
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "", Weight: 1},
				{RefName: "", TargetModel: "model-b", Weight: 1},
			},
			wantErr: false,
		},
		{
			name: "invalid item in slice (not a map)",
			obj: newExternalModel(map[string]interface{}{
				"externalProviderRefs": []interface{}{
					"not a map",
					map[string]interface{}{
						"ref":         map[string]interface{}{"name": "provider-a"},
						"targetModel": "model-a",
						"weight":      int64(100),
					},
				},
			}),
			wantRefs: []providerRef{
				{RefName: "provider-a", TargetModel: "model-a", Weight: 100},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRefs, err := getExternalProviderRefs(tt.obj)

			if tt.wantErr {
				if err == nil {
					t.Errorf("getExternalProviderRefs() expected error containing %q, got nil", tt.errContains)
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("getExternalProviderRefs() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("getExternalProviderRefs() unexpected error: %v", err)
				return
			}

			if len(gotRefs) != len(tt.wantRefs) {
				t.Errorf("getExternalProviderRefs() got %d refs, want %d", len(gotRefs), len(tt.wantRefs))
				return
			}

			for i, got := range gotRefs {
				want := tt.wantRefs[i]
				if got.RefName != want.RefName {
					t.Errorf("ref[%d].RefName = %q, want %q", i, got.RefName, want.RefName)
				}
				if got.TargetModel != want.TargetModel {
					t.Errorf("ref[%d].TargetModel = %q, want %q", i, got.TargetModel, want.TargetModel)
				}
				if got.Weight != want.Weight {
					t.Errorf("ref[%d].Weight = %d, want %d", i, got.Weight, want.Weight)
				}
			}
		})
	}
}

func TestExternalModelGVK(t *testing.T) {
	if externalModelGVK.Group != "inference.opendatahub.io" {
		t.Errorf("externalModelGVK.Group = %q, want %q", externalModelGVK.Group, "inference.opendatahub.io")
	}
	if externalModelGVK.Version != "v1alpha1" {
		t.Errorf("externalModelGVK.Version = %q, want %q", externalModelGVK.Version, "v1alpha1")
	}
	if externalModelGVK.Kind != "ExternalModel" {
		t.Errorf("externalModelGVK.Kind = %q, want %q", externalModelGVK.Kind, "ExternalModel")
	}
}

func newExternalModel(spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "inference.opendatahub.io/v1alpha1",
			"kind":       "ExternalModel",
			"metadata": map[string]interface{}{
				"name":      "test-model",
				"namespace": "test-ns",
			},
			"spec": spec,
		},
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
