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

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestAITenantResponsesSchema(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-controller", "crd", "bases", "maas.opendatahub.io_aitenants.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v1CRD apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &v1CRD); err != nil {
		t.Fatal(err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1CRD, &crd, nil); err != nil {
		t.Fatal(err)
	}
	schema, err := structuralschema.NewStructural(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	schemaValidator, _, err := validation.NewSchemaValidator(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(schema, true, celconfig.PerCallLimit)
	if celValidator == nil {
		t.Fatal("expected CEL validation rules")
	}

	tests := []struct {
		name      string
		responses map[string]any
		wantErr   string
	}{
		{name: "omitted"},
		{name: "empty", responses: map[string]any{}},
		{name: "disabled", responses: map[string]any{"enabled": false}},
		{name: "enabled", responses: map[string]any{"enabled": true}},
		{name: "empty storage", responses: map[string]any{"storage": map[string]any{}}},
		{name: "proposal example", responses: map[string]any{
			"enabled":   true,
			"storage":   map[string]any{"mode": "PlatformDefault", "deletionPolicy": "Retain"},
			"retention": map[string]any{"maxAge": "168h"},
		}},
		{name: "compound duration", responses: map[string]any{"retention": map[string]any{"maxAge": "1h30m"}}},
		{name: "fractional duration", responses: map[string]any{"retention": map[string]any{"maxAge": "0.5s"}}},
		{name: "invalid enabled", responses: map[string]any{"enabled": "true"}, wantErr: "spec.responses.enabled"},
		{name: "unsupported storage", responses: map[string]any{"storage": map[string]any{"mode": "SQLite"}}, wantErr: "spec.responses.storage.mode"},
		{name: "unsupported deletion", responses: map[string]any{"storage": map[string]any{"deletionPolicy": "Delete"}}, wantErr: "spec.responses.storage.deletionPolicy"},
		{name: "missing maxAge", responses: map[string]any{"retention": map[string]any{}}, wantErr: "spec.responses.retention.maxAge"},
		{name: "invalid duration", responses: map[string]any{"retention": map[string]any{"maxAge": "forever"}}, wantErr: "spec.responses.retention.maxAge"},
		{name: "negative duration", responses: map[string]any{"retention": map[string]any{"maxAge": "-1h"}}, wantErr: "spec.responses.retention.maxAge"},
		{name: "zero duration", responses: map[string]any{"retention": map[string]any{"maxAge": "0s"}}, wantErr: "maxAge must be a positive duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := map[string]any{}
			if tt.responses != nil {
				spec["responses"] = runtime.DeepCopyJSON(tt.responses)
			}
			obj := map[string]any{
				"apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "AITenant",
				"metadata": map[string]any{"name": "team-a", "namespace": "ai-tenants"}, "spec": spec,
			}
			pruning.Prune(obj, schema, true)
			defaulting.Default(obj, schema)
			errs := validation.ValidateCustomResource(nil, obj, schemaValidator)
			if len(errs) == 0 {
				celErrs, _ := celValidator.Validate(t.Context(), nil, schema, obj, nil, celconfig.RuntimeCELCostBudget)
				errs = append(errs, celErrs...)
			}
			if tt.wantErr != "" {
				if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tt.wantErr) {
					t.Fatalf("expected %q, got %v", tt.wantErr, errs.ToAggregate())
				}
				return
			}
			if len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if tt.responses == nil {
				if _, found := spec["responses"]; found {
					t.Fatal("omitted Responses must remain disabled and absent")
				}
				return
			}
			var tenant maasv1alpha1.AITenant
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &tenant); err != nil {
				t.Fatal(err)
			}
			wantEnabled, _ := tt.responses["enabled"].(bool)
			if tenant.Spec.Responses == nil || tenant.Spec.Responses.Enabled != wantEnabled {
				t.Fatalf("Responses switch was pruned or defaulted incorrectly: %+v", tenant.Spec.Responses)
			}
			if tenant.Spec.Responses.Storage.Mode != "PlatformDefault" || tenant.Spec.Responses.Storage.DeletionPolicy != "Retain" {
				t.Fatalf("unexpected storage defaults: %+v", tenant.Spec.Responses.Storage)
			}
			if retention, ok := tt.responses["retention"].(map[string]any); ok {
				if tenant.Spec.Responses.Retention == nil || tenant.Spec.Responses.Retention.MaxAge != retention["maxAge"] {
					t.Fatal("retention did not survive pruning and conversion")
				}
			}
			cloned := tenant.DeepCopy()
			if !reflect.DeepEqual(&tenant, cloned) {
				t.Fatal("deep copy lost Responses fields")
			}
			cloned.Spec.Responses.Enabled = !wantEnabled
			if cloned.Spec.Responses.Retention != nil {
				cloned.Spec.Responses.Retention.MaxAge = "1s"
			}
			if tenant.Spec.Responses.Enabled != wantEnabled || reflect.DeepEqual(tenant.Spec.Responses, cloned.Spec.Responses) {
				t.Fatal("deep copy shares Responses pointers")
			}
			if retention, ok := tt.responses["retention"].(map[string]any); ok && tenant.Spec.Responses.Retention.MaxAge != retention["maxAge"] {
				t.Fatal("deep copy shares the retention pointer")
			}
		})
	}
}
