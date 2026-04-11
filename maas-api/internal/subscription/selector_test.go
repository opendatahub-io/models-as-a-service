package subscription_test

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

const (
	defaultTestTokenRateLimit int64 = 1000
	phaseActive                     = "Active"
	phaseFailed                     = "Failed"
	phasePending                    = "Pending"
	phaseDegraded                   = "Degraded"
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

func createSubscription(name string, groups []string, users []string, priority int32, tokenLimit int64, displayName, description string) *unstructured.Unstructured {
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
						"limit":  tokenLimit,
						"window": "1m",
					},
				},
			},
		},
	}

	metadata := map[string]any{
		"name":      name,
		"namespace": "test-ns",
	}

	// Add optional displayName and description as annotations
	if displayName != "" || description != "" {
		annotations := map[string]any{}
		if displayName != "" {
			annotations["openshift.io/display-name"] = displayName
		}
		if description != "" {
			annotations["openshift.io/description"] = description
		}
		metadata["annotations"] = annotations
	}

	// Add Active status by default (real subscriptions are reconciled)
	status := map[string]any{
		"phase": phaseActive,
		"conditions": []any{
			map[string]any{
				"type":   "Ready",
				"status": "True",
			},
		},
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata":   metadata,
			"spec":       spec,
			"status":     status,
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
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, "Basic Tier", "Basic subscription for all users"),
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
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, "Basic Tier", "Basic subscription"),
				createSubscription("premium-sub", []string{"premium-users"}, nil, 20, defaultTestTokenRateLimit, "Premium Tier", "Premium subscription"),
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
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, "", ""),
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
				createSubscription("alice-sub", []string{}, []string{"alice"}, 10, defaultTestTokenRateLimit, "Alice's Subscription", "Personal subscription for Alice"),
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
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, "", ""),
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
				createSubscription("basic-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, "", ""),
				createSubscription("premium-sub", []string{"premium-users"}, nil, 20, defaultTestTokenRateLimit, "", ""),
				createSubscription("admin-sub", []string{"admin-users"}, nil, 30, defaultTestTokenRateLimit, "", ""),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"basic-sub"},
		},
		{
			name: "exclude Failed subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithHealth("failed-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, phaseFailed, false, false),
				createSubscriptionWithHealth("active-sub", []string{"basic-users"}, nil, 20, defaultTestTokenRateLimit, phaseActive, true, false),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"active-sub"},
		},
		{
			name: "exclude Pending subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithHealth("pending-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, phasePending, false, false),
				createSubscriptionWithHealth("active-sub", []string{"basic-users"}, nil, 20, defaultTestTokenRateLimit, phaseActive, true, false),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"active-sub"},
		},
		{
			name: "include Degraded subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithHealth("degraded-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, phaseDegraded, true, false),
				createSubscriptionWithHealth("active-sub", []string{"basic-users"}, nil, 20, defaultTestTokenRateLimit, phaseActive, true, false),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 2,
			expectedNames: []string{"active-sub", "degraded-sub"},
		},
		{
			name: "exclude deleting subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithHealth("deleting-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, phaseActive, true, true),
				createSubscriptionWithHealth("active-sub", []string{"basic-users"}, nil, 20, defaultTestTokenRateLimit, phaseActive, true, false),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 1,
			expectedNames: []string{"active-sub"},
		},
		{
			name: "filter by phase - only Active and Degraded included",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithHealth("active-sub", []string{"basic-users"}, nil, 10, defaultTestTokenRateLimit, phaseActive, true, false),
				createSubscriptionWithHealth("degraded-sub", []string{"basic-users"}, nil, 20, defaultTestTokenRateLimit, phaseDegraded, true, false),
				createSubscriptionWithHealth("failed-sub", []string{"basic-users"}, nil, 30, defaultTestTokenRateLimit, phaseFailed, false, false),
				createSubscriptionWithHealth("pending-sub", []string{"basic-users"}, nil, 40, defaultTestTokenRateLimit, phasePending, false, false),
			},
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedCount: 2,
			expectedNames: []string{"active-sub", "degraded-sub"},
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

func TestSelectHighestPriority(t *testing.T) {
	log := logger.New(false)

	t.Run("picks highest priority", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
			createSubscription("low-sub", []string{"g1"}, nil, 10, defaultTestTokenRateLimit, "L", "d1"),
			createSubscription("high-sub", []string{"g1"}, nil, 50, defaultTestTokenRateLimit, "H", "d2"),
		}}
		sel := subscription.NewSelector(log, lister)
		got, err := sel.SelectHighestPriority([]string{"g1"}, "")
		if err != nil {
			t.Fatalf("SelectHighestPriority: %v", err)
		}
		if got.Name != "high-sub" {
			t.Errorf("expected high-sub, got %q", got.Name)
		}
	})

	t.Run("tie on priority uses maxLimit then name", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
			createSubscription("sub-a", []string{"g1"}, nil, 10, 10, "", ""),
			createSubscription("sub-b", []string{"g1"}, nil, 10, 20, "", ""),
		}}
		sel := subscription.NewSelector(log, lister)
		got, err := sel.SelectHighestPriority([]string{"g1"}, "")
		if err != nil {
			t.Fatalf("SelectHighestPriority: %v", err)
		}
		if got.Name != "sub-b" {
			t.Errorf("expected sub-b (higher maxLimit), got %q", got.Name)
		}
	})

	t.Run("tie on priority and maxLimit uses name asc", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
			createSubscription("zebra", []string{"g1"}, nil, 5, defaultTestTokenRateLimit, "", ""),
			createSubscription("alpha", []string{"g1"}, nil, 5, defaultTestTokenRateLimit, "", ""),
		}}
		sel := subscription.NewSelector(log, lister)
		got, err := sel.SelectHighestPriority([]string{"g1"}, "")
		if err != nil {
			t.Fatalf("SelectHighestPriority: %v", err)
		}
		if got.Name != "alpha" {
			t.Errorf("expected alpha (lexicographic tie-break), got %q", got.Name)
		}
	})

	t.Run("no accessible subscription", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
			createSubscription("other", []string{"other-group"}, nil, 10, defaultTestTokenRateLimit, "", ""),
		}}
		sel := subscription.NewSelector(log, lister)
		_, err := sel.SelectHighestPriority([]string{"g1"}, "")
		if err == nil {
			t.Fatal("expected error")
		}
		var noSub *subscription.NoSubscriptionError
		if !errors.As(err, &noSub) {
			t.Fatalf("expected NoSubscriptionError, got %T %v", err, err)
		}
	})
}

// createSubscriptionWithHealth creates a subscription with health status fields.
//
//nolint:unparam // Test helper - parameters provide flexibility for future tests
func createSubscriptionWithHealth(
	name string, groups []string, users []string, priority int32,
	tokenLimit int64, phase string, ready bool, deleting bool,
) *unstructured.Unstructured {
	sub := createSubscription(name, groups, users, priority, tokenLimit, "", "")

	// Add status
	if phase != "" || ready {
		status := map[string]any{}
		if phase != "" {
			status["phase"] = phase
		}

		// Add Ready condition
		if phase != "" {
			conditions := []any{
				map[string]any{
					"type": "Ready",
					"status": func() string {
						if ready {
							return "True"
						}
						return "False"
					}(),
					"reason":  "Test",
					"message": "Test condition",
				},
			}
			status["conditions"] = conditions
		}

		sub.Object["status"] = status
	}

	// Add deletionTimestamp if deleting
	if deleting {
		metadata, ok := sub.Object["metadata"].(map[string]any)
		if !ok {
			panic("metadata should be map[string]any")
		}
		metadata["deletionTimestamp"] = "2026-04-08T12:00:00Z"
	}

	return sub
}

// createSubscriptionWithModelHealth creates a subscription with per-model health status.
func createSubscriptionWithModelHealth(name string, groups []string, phase string, modelStatuses []map[string]any) *unstructured.Unstructured {
	// Create base subscription
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	// Build modelRefs from modelStatuses
	modelRefs := make([]any, len(modelStatuses))
	for i, modelStatus := range modelStatuses {
		modelRefs[i] = map[string]any{
			"name":      modelStatus["name"],
			"namespace": modelStatus["namespace"],
			"tokenRateLimits": []any{
				map[string]any{
					"limit":  int64(1000),
					"window": "1m",
				},
			},
		}
	}

	spec := map[string]any{
		"owner": map[string]any{
			"groups": groupsSlice,
		},
		"priority":  int64(10),
		"modelRefs": modelRefs,
	}

	metadata := map[string]any{
		"name":      name,
		"namespace": "test-ns",
	}

	sub := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata":   metadata,
			"spec":       spec,
		},
	}

	// Add status
	status := map[string]any{
		"phase": phase,
		"conditions": []any{
			map[string]any{
				"type": "Ready",
				"status": func() string {
					if phase == phaseActive {
						return "True"
					}
					return "False"
				}(),
				"reason":  "Test",
				"message": "Test condition",
			},
		},
	}

	if len(modelStatuses) > 0 {
		// Convert to []any to avoid deep copy issues
		anyStatuses := make([]any, len(modelStatuses))
		for i, modelStatus := range modelStatuses {
			anyStatuses[i] = modelStatus
		}
		status["modelRefStatuses"] = anyStatuses
	}

	sub.Object["status"] = status
	return sub
}

func TestSelector_HealthFieldParsing(t *testing.T) {
	log := logger.New(false)

	tests := []struct {
		name             string
		subscription     *unstructured.Unstructured
		expectedPhase    string
		expectedReady    bool
		expectedDeleting bool
		expectError      bool // Failed/Pending subscriptions should error
	}{
		{
			name:             "Active subscription with Ready=True",
			subscription:     createSubscriptionWithHealth("active-sub", []string{"g1"}, nil, 10, 1000, phaseActive, true, false),
			expectedPhase:    phaseActive,
			expectedReady:    true,
			expectedDeleting: false,
			expectError:      false,
		},
		{
			name:             "Failed subscription with Ready=False - rejected",
			subscription:     createSubscriptionWithHealth("failed-sub", []string{"g1"}, nil, 10, 1000, phaseFailed, false, false),
			expectedPhase:    phaseFailed,
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      true, // Failed subscriptions are rejected
		},
		{
			name:             "Pending subscription with Ready=False - rejected",
			subscription:     createSubscriptionWithHealth("pending-sub", []string{"g1"}, nil, 10, 1000, phasePending, false, false),
			expectedPhase:    phasePending,
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      true, // Pending subscriptions are rejected
		},
		{
			name:             "Degraded subscription with Ready=False",
			subscription:     createSubscriptionWithHealth("degraded-sub", []string{"g1"}, nil, 10, 1000, phaseDegraded, false, false),
			expectedPhase:    phaseDegraded,
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      false,
		},
		{
			name:             "Subscription being deleted",
			subscription:     createSubscriptionWithHealth("deleting-sub", []string{"g1"}, nil, 10, 1000, phaseActive, true, true),
			expectedPhase:    phaseActive,
			expectedReady:    true,
			expectedDeleting: true,
			expectError:      false,
		},
		{
			name: "Subscription without status - rejected (unreconciled)",
			subscription: func() *unstructured.Unstructured {
				// Create subscription without status (unreconciled)
				return &unstructured.Unstructured{
					Object: map[string]any{
						"apiVersion": "maas.opendatahub.io/v1alpha1",
						"kind":       "MaaSSubscription",
						"metadata": map[string]any{
							"name":      "no-status-sub",
							"namespace": "test-ns",
						},
						"spec": map[string]any{
							"owner": map[string]any{
								"groups": []any{map[string]any{"name": "g1"}},
								"users":  []any{},
							},
							"priority": int64(10),
							"modelRefs": []any{
								map[string]any{
									"name": "test-model",
									"tokenRateLimits": []any{
										map[string]any{
											"limit":  int64(1000),
											"window": "1m",
										},
									},
								},
							},
						},
						// No status field - simulates unreconciled subscription
					},
				}
			}(),
			expectedPhase:    "",
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      true, // Empty phase means unreconciled - now rejected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: []*unstructured.Unstructured{tt.subscription}}
			selector := subscription.NewSelector(log, lister)

			//nolint:unqueryvet // False positive - not a SQL query
			result, err := selector.Select([]string{"g1"}, "", "", "")

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error for %s subscription, got nil", tt.expectedPhase)
				}
				// Error expected - test passes
				return
			}

			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}

			if result.Phase != tt.expectedPhase {
				t.Errorf("Phase = %v, want %v", result.Phase, tt.expectedPhase)
			}

			if result.Ready != tt.expectedReady {
				t.Errorf("Ready = %v, want %v", result.Ready, tt.expectedReady)
			}

			gotDeleting := result.DeletionTimestamp != ""
			if gotDeleting != tt.expectedDeleting {
				t.Errorf("DeletionTimestamp set = %v, want %v", gotDeleting, tt.expectedDeleting)
			}
		})
	}
}

func TestSelector_ListAccessibleWithHealth(t *testing.T) {
	log := logger.New(false)

	subscriptions := []*unstructured.Unstructured{
		createSubscriptionWithHealth("active-sub", []string{"g1"}, nil, 10, 1000, phaseActive, true, false),
		createSubscriptionWithHealth("degraded-sub", []string{"g1"}, nil, 9, 1000, phaseDegraded, true, false),
		createSubscriptionWithHealth("failed-sub", []string{"g1"}, nil, 5, 1000, phaseFailed, false, false),
		createSubscriptionWithHealth("deleting-sub", []string{"g1"}, nil, 8, 1000, phaseActive, true, true),
	}

	lister := &fakeLister{subscriptions: subscriptions}
	selector := subscription.NewSelector(log, lister)

	results, err := selector.GetAllAccessible([]string{"g1"}, "")
	if err != nil {
		t.Fatalf("GetAllAccessible() error = %v", err)
	}

	// Only Active and Degraded subscriptions are returned (Failed and deleting are filtered out)
	if len(results) != 2 {
		t.Fatalf("Expected 2 subscriptions (Active and Degraded only), got %d", len(results))
	}

	// Check that health fields are populated in returned results
	for _, result := range results {
		switch result.Name {
		case "active-sub":
			if result.Phase != phaseActive || !result.Ready || result.DeletionTimestamp != "" {
				t.Errorf("active-sub health fields incorrect: Phase=%s, Ready=%v, DeletionTimestamp=%s",
					result.Phase, result.Ready, result.DeletionTimestamp)
			}
		case "degraded-sub":
			if result.Phase != phaseDegraded || !result.Ready || result.DeletionTimestamp != "" {
				t.Errorf("degraded-sub health fields incorrect: Phase=%s, Ready=%v, DeletionTimestamp=%s",
					result.Phase, result.Ready, result.DeletionTimestamp)
			}
		case "failed-sub":
			t.Errorf("failed-sub should have been filtered out")
		case "deleting-sub":
			t.Errorf("deleting-sub should have been filtered out")
		}
	}
}

func TestSelector_DegradedSubscriptionActiveFiltering(t *testing.T) {
	log := logger.New(false)

	tests := []struct {
		name              string
		subscription      *unstructured.Unstructured
		requestedModel    string
		expectError       bool
		expectedErrorType string
	}{
		{
			name: "Degraded subscription with healthy model - allows access",
			subscription: createSubscriptionWithModelHealth("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "test-model",
					"namespace": "test-ns",
					"ready":     true,
					"reason":    "Available",
					"message":   "Model is healthy",
				},
			}),
			requestedModel: "test-ns/test-model",
			expectError:    false,
		},
		{
			name: "Degraded subscription with unhealthy model - rejects access",
			subscription: createSubscriptionWithModelHealth("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "test-model",
					"namespace": "test-ns",
					"ready":     false,
					"reason":    "ModelNotReady",
					"message":   "Model deployment failed",
				},
			}),
			requestedModel:    "test-ns/test-model",
			expectError:       true,
			expectedErrorType: "ModelUnhealthyError",
		},
		{
			name: "Degraded subscription with missing model status - rejects access (fail-closed)",
			subscription: func() *unstructured.Unstructured {
				sub := createSubscriptionWithModelHealth("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
					{
						"name":      "other-model",
						"namespace": "test-ns",
						"ready":     true,
						"reason":    "Available",
						"message":   "Other model is healthy",
					},
				})
				// Add test-model to modelRefs but not in status
				spec, ok := sub.Object["spec"].(map[string]any)
				if !ok {
					panic("spec should be map[string]any")
				}
				modelRefs, ok := spec["modelRefs"].([]any)
				if !ok {
					panic("modelRefs should be []any")
				}
				modelRefs = append(modelRefs, map[string]any{
					"name":      "test-model",
					"namespace": "test-ns",
					"tokenRateLimits": []any{
						map[string]any{
							"limit":  int64(1000),
							"window": "1m",
						},
					},
				})
				spec["modelRefs"] = modelRefs
				return sub
			}(),
			requestedModel:    "test-ns/test-model",
			expectError:       true,
			expectedErrorType: "ModelUnhealthyError",
		},
		{
			name: "Degraded subscription with empty modelRefStatuses - rejects access (fail-closed)",
			subscription: func() *unstructured.Unstructured {
				// Create a subscription with test-model in modelRefs but empty modelRefStatuses
				sub := createSubscription("degraded-sub", []string{"g1"}, nil, 10, 1000, "", "")
				// Update the modelRefs to include namespace
				spec, ok := sub.Object["spec"].(map[string]any)
				if !ok {
					panic("spec should be map[string]any")
				}
				spec["modelRefs"] = []any{
					map[string]any{
						"name":      "test-model",
						"namespace": "test-ns",
						"tokenRateLimits": []any{
							map[string]any{
								"limit":  int64(1000),
								"window": "1m",
							},
						},
					},
				}
				// Add Degraded phase status with empty modelRefStatuses
				sub.Object["status"] = map[string]any{
					"phase": phaseDegraded,
					"conditions": []any{
						map[string]any{
							"type":    "Ready",
							"status":  "False",
							"reason":  phaseDegraded,
							"message": "Some models unhealthy",
						},
					},
					// Empty modelRefStatuses - should be rejected for Degraded subscription
				}
				return sub
			}(),
			requestedModel:    "test-ns/test-model",
			expectError:       true,
			expectedErrorType: "ModelUnhealthyError",
		},
		{
			name: "Active subscription - always allows access regardless of model health",
			subscription: createSubscriptionWithModelHealth("active-sub", []string{"g1"}, phaseActive, []map[string]any{
				{
					"name":      "test-model",
					"namespace": "test-ns",
					"ready":     false,
					"reason":    "ModelNotReady",
					"message":   "Model is broken",
				},
			}),
			requestedModel: "test-ns/test-model",
			expectError:    false,
		},
		{
			name: "Degraded subscription without model specified - allows access",
			subscription: createSubscriptionWithModelHealth("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "test-model",
					"namespace": "test-ns",
					"ready":     false,
					"reason":    "ModelNotReady",
					"message":   "Model is broken",
				},
			}),
			requestedModel: "",
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: []*unstructured.Unstructured{tt.subscription}}
			selector := subscription.NewSelector(log, lister)

			//nolint:unqueryvet // False positive - not a SQL query
			result, err := selector.Select([]string{"g1"}, "", "", tt.requestedModel)

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error but got none")
				}
				if tt.expectedErrorType == "ModelUnhealthyError" {
					var modelUnhealthyErr *subscription.ModelUnhealthyError
					if !errors.As(err, &modelUnhealthyErr) {
						t.Fatalf("Expected ModelUnhealthyError, got %T: %v", err, err)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("Expected no error but got: %v", err)
				}
				if result == nil {
					t.Fatal("Expected result but got nil")
				}
			}
		})
	}
}
