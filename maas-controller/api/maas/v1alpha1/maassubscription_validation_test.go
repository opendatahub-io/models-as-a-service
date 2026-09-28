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
	"strconv"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
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

// Covers the guardrails attachment example: subscription-wide guardrails plus a
// per-model guardrails entry must be recognized by the generated CRD schema
// (survive pruning) and satisfy its CEL rules.
func TestMaaSSubscriptionGuardrailsAttachmentsAccepted(t *testing.T) {
	schema, validator := maaSSubscriptionValidator(t)

	obj := map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSSubscription",
		"metadata":   map[string]any{"name": "application-subscription", "namespace": "tenant-ns"},
		"spec": map[string]any{
			"owner": map[string]any{"groups": []any{map[string]any{"name": "team"}}},
			"guardrails": []any{map[string]any{
				"ref":    map[string]any{"name": "application-safety-v1"},
				"checks": []any{"subscription-check"},
			}},
			"modelRefs": []any{map[string]any{
				"name":      "granite-7b",
				"namespace": "model-ns",
				"unlimited": true, // required by the existing CEL rule; not part of guardrails
				"guardrails": []any{map[string]any{
					"ref":    map[string]any{"name": "application-safety-v1"},
					"checks": []any{"application-check"},
				}},
			}},
		},
	}

	pruned := runtime.DeepCopyJSON(obj)
	pruning.Prune(pruned, schema, true)
	spec, ok := pruned["spec"].(map[string]any)
	if !ok {
		t.Fatal("pruned object is missing spec")
	}
	if _, ok := spec["guardrails"]; !ok {
		t.Fatal("spec.guardrails was pruned — field missing from generated CRD schema")
	}
	modelRefs, ok := spec["modelRefs"].([]any)
	if !ok || len(modelRefs) == 0 {
		t.Fatal("pruned object is missing modelRefs")
	}
	model, ok := modelRefs[0].(map[string]any)
	if !ok {
		t.Fatal("pruned modelRefs[0] is not an object")
	}
	if _, ok := model["guardrails"]; !ok {
		t.Fatal("spec.modelRefs[].guardrails was pruned — field missing from generated CRD schema")
	}

	errs, _ := validator.Validate(t.Context(), nil, schema, obj, nil, celconfig.RuntimeCELCostBudget)
	if len(errs) > 0 {
		t.Fatalf("expected no validation errors, got %v", errs.ToAggregate())
	}
}

// A namespace is intentionally not part of a guardrail reference: the policy
// resolves in the tenant target namespace only. A namespace supplied on the ref
// must be dropped by the schema rather than honored.
func TestMaaSSubscriptionGuardrailRefNamespaceIsPruned(t *testing.T) {
	schema, _ := maaSSubscriptionValidator(t)

	obj := map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1",
		"kind":       "MaaSSubscription",
		"metadata":   map[string]any{"name": "sub", "namespace": "tenant-ns"},
		"spec": map[string]any{
			"owner": map[string]any{"groups": []any{map[string]any{"name": "team"}}},
			"guardrails": []any{map[string]any{
				"ref": map[string]any{"name": "policy-v1", "namespace": "attacker-ns"},
			}},
			"modelRefs": []any{map[string]any{"name": "granite-7b", "namespace": "model-ns", "unlimited": true}},
		},
	}

	pruning.Prune(obj, schema, true)
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatal("pruned object is missing spec")
	}
	guardrails, ok := spec["guardrails"].([]any)
	if !ok || len(guardrails) == 0 {
		t.Fatal("pruned object is missing guardrails")
	}
	attachment, ok := guardrails[0].(map[string]any)
	if !ok {
		t.Fatal("pruned guardrails[0] is not an object")
	}
	ref, ok := attachment["ref"].(map[string]any)
	if !ok {
		t.Fatal("pruned guardrails[0].ref is not an object")
	}
	if _, ok := ref["namespace"]; ok {
		t.Fatalf("guardrail ref.namespace should be pruned, got %v", ref)
	}
}

// Value-shape violations on guardrail attachments must be rejected by the
// generated CRD's structural schema (independent of the CEL rules).
func TestMaaSSubscriptionGuardrailsInvalidAttachmentsRejected(t *testing.T) {
	schemaValidator := maaSSubscriptionSchemaValidator(t)

	manyChecks := make([]any, 0, 65)
	for i := range 65 {
		manyChecks = append(manyChecks, "check-"+strconv.Itoa(i))
	}

	tests := []struct {
		name       string
		guardrails []any
		wantErr    string
	}{
		{
			name:       "ref name violates DNS-1123 pattern",
			guardrails: []any{map[string]any{"ref": map[string]any{"name": "Invalid_Name"}}},
			wantErr:    "spec.guardrails[0].ref.name",
		},
		{
			name:       "ref name empty",
			guardrails: []any{map[string]any{"ref": map[string]any{"name": ""}}},
			wantErr:    "spec.guardrails[0].ref.name",
		},
		{
			name:       "missing ref",
			guardrails: []any{map[string]any{"checks": []any{"a-check"}}},
			wantErr:    "spec.guardrails[0].ref",
		},
		{
			name: "check name violates pattern",
			guardrails: []any{map[string]any{
				"ref":    map[string]any{"name": "policy-v1"},
				"checks": []any{"Bad Check"},
			}},
			wantErr: "spec.guardrails[0].checks[0]",
		},
		{
			name: "too many checks",
			guardrails: []any{map[string]any{
				"ref":    map[string]any{"name": "policy-v1"},
				"checks": manyChecks,
			}},
			wantErr: "spec.guardrails[0].checks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := map[string]any{
				"apiVersion": "maas.opendatahub.io/v1alpha1",
				"kind":       "MaaSSubscription",
				"metadata":   map[string]any{"name": "sub", "namespace": "tenant-ns"},
				"spec": map[string]any{
					"owner":      map[string]any{"groups": []any{map[string]any{"name": "team"}}},
					"guardrails": tt.guardrails,
					"modelRefs":  []any{map[string]any{"name": "granite-7b", "namespace": "model-ns", "unlimited": true}},
				},
			}

			errs := validation.ValidateCustomResource(nil, obj, schemaValidator)
			if len(errs) == 0 {
				t.Fatalf("expected a validation error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(errs.ToAggregate().Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, errs.ToAggregate())
			}
		})
	}
}

func maaSSubscriptionSchemaValidator(t *testing.T) validation.SchemaValidator {
	t.Helper()
	crd := loadMaaSSubscriptionCRD(t)
	schemaValidator, _, err := validation.NewSchemaValidator(crd.Spec.Validation.OpenAPIV3Schema)
	if err != nil {
		t.Fatalf("build schema validator: %v", err)
	}
	return schemaValidator
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
