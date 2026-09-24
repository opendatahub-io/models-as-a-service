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
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/runtime"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// Evaluates the CEL rules of the generated MaaSSubscription CRD the way the
// API server does, so the rules are exercised without a cluster.
func TestMaaSSubscriptionModelRefTokenBudgetRules(t *testing.T) {
	limits := []any{map[string]any{"limit": int64(100), "window": "1m"}}

	tests := []struct {
		name    string
		ref     map[string]any
		wantErr string
	}{
		{
			name: "token rate limits only",
			ref:  map[string]any{"tokenRateLimits": limits},
		},
		{
			name: "unlimited only",
			ref:  map[string]any{"unlimited": true},
		},
		{
			name: "unlimited false with token rate limits",
			ref:  map[string]any{"unlimited": false, "tokenRateLimits": limits},
		},
		{
			name:    "neither set",
			ref:     map[string]any{},
			wantErr: "tokenRateLimits is required unless unlimited is true",
		},
		{
			name:    "unlimited false without token rate limits",
			ref:     map[string]any{"unlimited": false},
			wantErr: "tokenRateLimits is required unless unlimited is true",
		},
		{
			name:    "both set",
			ref:     map[string]any{"unlimited": true, "tokenRateLimits": limits},
			wantErr: "tokenRateLimits must not be set when unlimited is true",
		},
	}

	schema, validator := maaSSubscriptionValidator(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := map[string]any{"name": "model", "namespace": "llm"}
			for k, v := range tt.ref {
				ref[k] = v
			}
			obj := maaSSubscriptionObject(ref)

			errs, _ := validator.Validate(t.Context(), nil, schema, obj, nil, celconfig.RuntimeCELCostBudget)

			if tt.wantErr == "" {
				if len(errs) > 0 {
					t.Fatalf("expected no validation errors, got %v", errs.ToAggregate())
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.wantErr) {
				t.Fatalf("expected a single %q error, got %v", tt.wantErr, errs.ToAggregate())
			}
		})
	}
}

// The Go type must serialize an unlimited ref without tokenRateLimits, or the
// controller and maas-api could not write one back.
func TestMaaSSubscriptionUnlimitedRefSerializesWithinRules(t *testing.T) {
	sub := maasv1alpha1.ModelSubscriptionRef{Name: "model", Namespace: "llm", Unlimited: true}
	ref, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&sub)
	if err != nil {
		t.Fatalf("convert ModelSubscriptionRef: %v", err)
	}
	if _, found := ref["tokenRateLimits"]; found {
		t.Fatalf("expected tokenRateLimits to be omitted, got %v", ref)
	}

	schema, validator := maaSSubscriptionValidator(t)
	errs, _ := validator.Validate(t.Context(), nil, schema, maaSSubscriptionObject(ref), nil, celconfig.RuntimeCELCostBudget)
	if len(errs) > 0 {
		t.Fatalf("expected no validation errors, got %v", errs.ToAggregate())
	}
}

func maaSSubscriptionObject(ref map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSSubscription",
		"metadata":   map[string]any{"name": "sub", "namespace": "models-as-a-service"},
		"spec": map[string]any{
			"owner":     map[string]any{"groups": []any{map[string]any{"name": "team"}}},
			"modelRefs": []any{ref},
		},
	}
}

func maaSSubscriptionValidator(t *testing.T) (*structuralschema.Structural, *cel.Validator) {
	t.Helper()
	crd := loadMaaSSubscriptionCRD(t)
	// Conversion to the internal type hoists a schema shared by all versions
	// into spec.validation.
	schema, err := structuralschema.NewStructural(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}
	validator := cel.NewValidator(schema, true, celconfig.PerCallLimit)
	if validator == nil {
		t.Fatal("expected the MaaSSubscription CRD to declare CEL rules")
	}
	return schema, validator
}

func loadMaaSSubscriptionCRD(t *testing.T) *apiextensions.CustomResourceDefinition {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-controller", "crd", "bases", "maas.opendatahub.io_maassubscriptions.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var v1CRD apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &v1CRD); err != nil {
		t.Fatalf("decode CRD: %v", err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1CRD, &crd, nil); err != nil {
		t.Fatalf("convert CRD: %v", err)
	}
	return &crd
}
