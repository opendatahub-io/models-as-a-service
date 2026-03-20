package subscription_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// fakeLister implements subscription.Lister for testing.
type fakeLister struct {
	subscriptions []*unstructured.Unstructured
	err           error
}

func (f *fakeLister) List() ([]*unstructured.Unstructured, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.subscriptions, nil
}

func createSubscription(name string, groups []string, users []string, priority int32, displayName, description string) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	usersSlice := make([]any, len(users))
	for i, u := range users {
		usersSlice[i] = u
	}

	spec := map[string]any{
		"owner": map[string]any{
			"groups": groupsSlice,
			"users":  usersSlice,
		},
		"priority": int64(priority),
		"modelRefs": []any{
			map[string]any{
				"name": "test-model",
				"tokenRateLimits": []any{
					map[string]any{
						"limit": int64(1000),
					},
				},
			},
		},
	}

	// Add optional displayName and description
	if displayName != "" {
		spec["displayName"] = displayName
	}
	if description != "" {
		spec["description"] = description
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "test-ns",
			},
			"spec": spec,
		},
	}
}

func TestGetAllAccessible(t *testing.T) {
	log := logger.New(false)

	tests := []struct {
		name                 string
		subscriptions        []*unstructured.Unstructured
		groups               []string
		username             string
		expectedCount        int
		expectedNames        []string
		expectedDisplayNames map[string]string // map[name]displayName
		expectedDescriptions map[string]string // map[name]description
		expectError          bool
	}{
		{
			name: "user has access to single subscription",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, "Basic Tier", "Basic subscription for all users"),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"basic-sub"},
			expectedDisplayNames: map[string]string{
				"basic-sub": "Basic Tier",
			},
			expectedDescriptions: map[string]string{
				"basic-sub": "Basic subscription for all users",
			},
		},
		{
			name: "user has access to multiple subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, "Basic Tier", "Basic subscription"),
				createSubscription("premium-sub", []string{"premium-users"}, nil, 20, "Premium Tier", "Premium subscription"),
			},
			groups:        []string{"basic-users", "premium-users"},
			username:      "alice",
			expectedCount: 2,
			expectedNames: []string{"basic-sub", "premium-sub"},
			expectedDisplayNames: map[string]string{
				"basic-sub":   "Basic Tier",
				"premium-sub": "Premium Tier",
			},
			expectedDescriptions: map[string]string{
				"basic-sub":   "Basic subscription",
				"premium-sub": "Premium subscription",
			},
		},
		{
			name: "user has no subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, "", ""),
			},
			groups:        []string{"other-users"},
			username:      "alice",
			expectedCount: 0,
			expectedNames: []string{},
		},
		{
			name:          "no subscriptions exist",
			subscriptions: []*unstructured.Unstructured{},
			groups:        []string{"any-group"},
			username:      "alice",
			expectedCount: 0,
			expectedNames: []string{},
		},
		{
			name: "user matched by username",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("alice-sub", []string{}, []string{"alice"}, 10, "Alice's Subscription", "Personal subscription for Alice"),
			},
			groups:        []string{},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"alice-sub"},
			expectedDisplayNames: map[string]string{
				"alice-sub": "Alice's Subscription",
			},
			expectedDescriptions: map[string]string{
				"alice-sub": "Personal subscription for Alice",
			},
		},
		{
			name: "subscriptions without displayName and description",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, "", ""),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"basic-sub"},
			expectedDisplayNames: map[string]string{
				"basic-sub": "", // Should be empty
			},
			expectedDescriptions: map[string]string{
				"basic-sub": "", // Should be empty
			},
		},
		{
			name: "filter out subscriptions user doesn't have access to",
			subscriptions: []*unstructured.Unstructured{
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, "", ""),
				createSubscription("premium-sub", []string{"premium-users"}, nil, 20, "", ""),
				createSubscription("admin-sub", []string{"admin-users"}, nil, 30, "", ""),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"basic-sub"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: tt.subscriptions}
			selector := subscription.NewSelector(log, lister)

			result, err := selector.GetAllAccessible(tt.groups, tt.username)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if len(result) != tt.expectedCount {
				t.Errorf("expected %d subscriptions, got %d", tt.expectedCount, len(result))
				return
			}

			// Verify subscription names
			gotNames := make(map[string]bool)
			for _, sub := range result {
				gotNames[sub.Name] = true
			}

			for _, expectedName := range tt.expectedNames {
				if !gotNames[expectedName] {
					t.Errorf("expected subscription %q not found in results", expectedName)
				}
			}

			// Verify displayNames and descriptions if provided
			if tt.expectedDisplayNames != nil {
				for _, sub := range result {
					expectedDisplayName := tt.expectedDisplayNames[sub.Name]
					if sub.DisplayName != expectedDisplayName {
						t.Errorf("subscription %q: expected displayName %q, got %q", sub.Name, expectedDisplayName, sub.DisplayName)
					}
				}
			}

			if tt.expectedDescriptions != nil {
				for _, sub := range result {
					expectedDescription := tt.expectedDescriptions[sub.Name]
					if sub.Description != expectedDescription {
						t.Errorf("subscription %q: expected description %q, got %q", sub.Name, expectedDescription, sub.Description)
					}
				}
			}
		})
	}
}

func TestGetAllAccessible_ErrorHandling(t *testing.T) {
	log := logger.New(false)

	t.Run("requires groups or username", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{}}
		selector := subscription.NewSelector(log, lister)

		_, err := selector.GetAllAccessible(nil, "")
		if err == nil {
			t.Error("expected error when both groups and username are empty")
		}
		if err.Error() != "either groups or username must be provided" {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}
