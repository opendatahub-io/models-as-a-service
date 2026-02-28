/*
Copyright 2025.

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

package maas

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMaaSModelReconciler_gatewayName(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayName(); got != defaultGatewayName {
			t.Errorf("gatewayName() = %q, want %q", got, defaultGatewayName)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayName: "my-gateway"}
		if got := r.gatewayName(); got != "my-gateway" {
			t.Errorf("gatewayName() = %q, want %q", got, "my-gateway")
		}
	})
}

func TestMaaSModelReconciler_gatewayNamespace(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayNamespace(); got != defaultGatewayNamespace {
			t.Errorf("gatewayNamespace() = %q, want %q", got, defaultGatewayNamespace)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayNamespace: "my-ns"}
		if got := r.gatewayNamespace(); got != "my-ns" {
			t.Errorf("gatewayNamespace() = %q, want %q", got, "my-ns")
		}
	})
}

// modelDeleteTestRESTMapper builds a REST mapper for the Kuadrant GVKs exercised by
// deleteGeneratedPoliciesByLabel. Neither AuthPolicy nor TokenRateLimitPolicy is
// registered in the scheme, so a custom mapper is required.
func modelDeleteTestRESTMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper(nil)
	ns := nsRestScope{}
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"}, ns)
	return m
}

// newPreexistingGeneratedPolicy builds an unstructured Kuadrant policy with the labels
// that deleteGeneratedPoliciesByLabel selects on. The name and GVK are caller-supplied
// so the same helper covers both AuthPolicy and TokenRateLimitPolicy.
func newPreexistingGeneratedPolicy(gvk schema.GroupVersionKind, name, namespace, modelName string, annotations map[string]string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(gvk)
	p.SetName(name)
	p.SetNamespace(namespace)
	p.SetLabels(map[string]string{
		"maas.opendatahub.io/model":    modelName,
		"app.kubernetes.io/managed-by": "maas-controller",
	})
	p.SetAnnotations(annotations)
	return p
}

// TestMaaSModelReconciler_DeleteGeneratedPolicies_ManagedAnnotation verifies that
// deleteGeneratedPoliciesByLabel respects the opt-out annotation on both
// AuthPolicy and TokenRateLimitPolicy resources when a MaaSModel is deleted.
func TestMaaSModelReconciler_DeleteGeneratedPolicies_ManagedAnnotation(t *testing.T) {
	const (
		modelName  = "llm"
		namespace  = "default"
		policyName = "test-policy"
	)

	resources := []struct {
		kind    string
		group   string
		version string
	}{
		{kind: "AuthPolicy", group: "kuadrant.io", version: "v1"},
		{kind: "TokenRateLimitPolicy", group: "kuadrant.io", version: "v1alpha1"},
	}

	cases := []struct {
		name        string
		annotations map[string]string
		wantDeleted bool
	}{
		{
			name:        "annotation absent: controller deletes",
			annotations: map[string]string{},
			wantDeleted: true,
		},
		{
			name:        "managed=true (new annotation): controller deletes",
			annotations: map[string]string{ManagedByODHOperator: "true"},
			wantDeleted: true,
		},
		{
			name:        "managed=false (new annotation): controller must not delete",
			annotations: map[string]string{ManagedByODHOperator: "false"},
			wantDeleted: false,
		},
		{
			name:        "managed=true (legacy annotation): controller deletes",
			annotations: map[string]string{ManagedByMaasODHOperator: "true"},
			wantDeleted: true,
		},
		{
			name:        "managed=false (legacy annotation): controller must not delete",
			annotations: map[string]string{ManagedByMaasODHOperator: "false"},
			wantDeleted: false,
		},
	}

	for _, res := range resources {
		t.Run(res.kind, func(t *testing.T) {
			gvk := schema.GroupVersionKind{Group: res.group, Version: res.version, Kind: res.kind}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					existing := newPreexistingGeneratedPolicy(gvk, policyName, namespace, modelName, tc.annotations)

					c := fake.NewClientBuilder().
						WithScheme(scheme).
						WithRESTMapper(modelDeleteTestRESTMapper()).
						WithObjects(existing).
						Build()

					r := &MaaSModelReconciler{Client: c, Scheme: scheme}
					if err := r.deleteGeneratedPoliciesByLabel(context.Background(), logr.Discard(), modelName, res.kind, res.group, res.version); err != nil {
						t.Fatalf("deleteGeneratedPoliciesByLabel: unexpected error: %v", err)
					}

					got := &unstructured.Unstructured{}
					got.SetGroupVersionKind(gvk)
					err := c.Get(context.Background(), types.NamespacedName{Name: policyName, Namespace: namespace}, got)

					if tc.wantDeleted {
						if !apierrors.IsNotFound(err) {
							t.Errorf("expected %s %q to be deleted, but it still exists", res.kind, policyName)
						}
					} else {
						if err != nil {
							t.Errorf("expected %s %q to survive deletion (managed=false opt-out), but got: %v", res.kind, policyName, err)
						}
					}
				})
			}
		})
	}
}
