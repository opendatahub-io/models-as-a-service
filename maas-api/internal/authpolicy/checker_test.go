package authpolicy_test

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type fakeLister struct {
	policies []*unstructured.Unstructured
	err      error
}

func (f *fakeLister) List() ([]*unstructured.Unstructured, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.policies, nil
}

func createPolicy(name string, users []string, groups []string, modelRefs []map[string]string) *unstructured.Unstructured {
	usersSlice := make([]any, len(users))
	for i, u := range users {
		usersSlice[i] = u
	}

	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	refs := make([]any, len(modelRefs))
	for i, ref := range modelRefs {
		m := map[string]any{}
		for k, v := range ref {
			m[k] = v
		}
		refs[i] = m
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSAuthPolicy",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"subjects": map[string]any{
					"users":  usersSlice,
					"groups": groupsSlice,
				},
				"modelRefs": refs,
			},
		},
	}
}

func TestAuthorizedModels(t *testing.T) {
	log := logger.New(false)

	tests := []struct {
		name             string
		policies         []*unstructured.Unstructured
		groups           []string
		username         string
		expectedModels   []authpolicy.ModelKey
		unexpectedModels []authpolicy.ModelKey
	}{
		{
			name: "user matched by username",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", []string{"alice"}, nil, []map[string]string{
					{"name": "model-a", "namespace": "llm"},
				}),
			},
			groups:   nil,
			username: "alice",
			expectedModels: []authpolicy.ModelKey{
				{Namespace: "llm", Name: "model-a"},
			},
		},
		{
			name: "user matched by group",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", nil, []string{"data-scientists"}, []map[string]string{
					{"name": "model-b", "namespace": "llm"},
				}),
			},
			groups:   []string{"data-scientists"},
			username: "bob",
			expectedModels: []authpolicy.ModelKey{
				{Namespace: "llm", Name: "model-b"},
			},
		},
		{
			name: "user has no access",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", []string{"alice"}, []string{"admins"}, []map[string]string{
					{"name": "model-a", "namespace": "llm"},
				}),
			},
			groups:   []string{"viewers"},
			username: "bob",
			unexpectedModels: []authpolicy.ModelKey{
				{Namespace: "llm", Name: "model-a"},
			},
		},
		{
			name: "multiple policies aggregate models",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", []string{"alice"}, nil, []map[string]string{
					{"name": "model-a", "namespace": "llm"},
				}),
				createPolicy("policy-2", []string{"alice"}, nil, []map[string]string{
					{"name": "model-b", "namespace": "llm"},
				}),
			},
			groups:   nil,
			username: "alice",
			expectedModels: []authpolicy.ModelKey{
				{Namespace: "llm", Name: "model-a"},
				{Namespace: "llm", Name: "model-b"},
			},
		},
		{
			name: "multiple models in single policy",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", nil, []string{"team-a"}, []map[string]string{
					{"name": "model-a", "namespace": "llm"},
					{"name": "model-b", "namespace": "llm"},
					{"name": "model-c", "namespace": "other"},
				}),
			},
			groups:   []string{"team-a"},
			username: "",
			expectedModels: []authpolicy.ModelKey{
				{Namespace: "llm", Name: "model-a"},
				{Namespace: "llm", Name: "model-b"},
				{Namespace: "other", Name: "model-c"},
			},
		},
		{
			name:           "no policies returns empty map",
			policies:       []*unstructured.Unstructured{},
			groups:         []string{"team-a"},
			username:       "alice",
			expectedModels: nil,
		},
		{
			name: "model ref without namespace",
			policies: []*unstructured.Unstructured{
				createPolicy("policy-1", []string{"alice"}, nil, []map[string]string{
					{"name": "model-a"},
				}),
			},
			groups:   nil,
			username: "alice",
			expectedModels: []authpolicy.ModelKey{
				{Namespace: "", Name: "model-a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{policies: tt.policies}
			checker := authpolicy.NewChecker(log, lister)

			result := checker.AuthorizedModels(tt.groups, tt.username)

			for _, key := range tt.expectedModels {
				if !result[key] {
					t.Errorf("expected model %v to be authorized, but it was not", key)
				}
			}
			for _, key := range tt.unexpectedModels {
				if result[key] {
					t.Errorf("expected model %v to NOT be authorized, but it was", key)
				}
			}
		})
	}
}

func TestAuthorizedModels_NilLister(t *testing.T) {
	log := logger.New(false)
	checker := authpolicy.NewChecker(log, nil)

	result := checker.AuthorizedModels([]string{"team-a"}, "alice")
	if result != nil {
		t.Errorf("expected nil result with nil lister, got %v", result)
	}
}

func TestAuthorizedModels_ListerError(t *testing.T) {
	log := logger.New(false)
	lister := &fakeLister{err: errors.New("connection refused")}
	checker := authpolicy.NewChecker(log, lister)

	result := checker.AuthorizedModels([]string{"team-a"}, "alice")
	if result != nil {
		t.Errorf("expected nil result on lister error, got %v", result)
	}
}

func TestIsModelAccessible(t *testing.T) {
	log := logger.New(false)

	policies := []*unstructured.Unstructured{
		createPolicy("policy-1", []string{"alice"}, []string{"team-a"}, []map[string]string{
			{"name": "model-a", "namespace": "llm"},
			{"name": "model-b", "namespace": "llm"},
		}),
	}
	lister := &fakeLister{policies: policies}
	checker := authpolicy.NewChecker(log, lister)

	tests := []struct {
		name           string
		groups         []string
		username       string
		modelName      string
		modelNamespace string
		expected       bool
	}{
		{
			name:           "accessible by username",
			username:       "alice",
			modelName:      "model-a",
			modelNamespace: "llm",
			expected:       true,
		},
		{
			name:           "accessible by group",
			groups:         []string{"team-a"},
			username:       "bob",
			modelName:      "model-b",
			modelNamespace: "llm",
			expected:       true,
		},
		{
			name:           "not accessible - wrong model",
			username:       "alice",
			modelName:      "model-c",
			modelNamespace: "llm",
			expected:       false,
		},
		{
			name:           "not accessible - wrong namespace",
			username:       "alice",
			modelName:      "model-a",
			modelNamespace: "other",
			expected:       false,
		},
		{
			name:           "not accessible - no matching subject",
			groups:         []string{"viewers"},
			username:       "bob",
			modelName:      "model-a",
			modelNamespace: "llm",
			expected:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := checker.IsModelAccessible(tt.groups, tt.username, tt.modelName, tt.modelNamespace)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
