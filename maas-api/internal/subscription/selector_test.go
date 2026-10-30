package subscription_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
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

func TestSelector_OrganizationIDPrecedence(t *testing.T) {
	tests := []struct {
		name               string
		organizationID     any
		tokenMetadataOrgID any
		expectedOrgID      string
	}{
		{
			name:               "stable organization overrides token metadata",
			organizationID:     "stable-org",
			tokenMetadataOrgID: "legacy-org",
			expectedOrgID:      "stable-org",
		},
		{
			name:               "stable organization defaults missing token metadata",
			organizationID:     "stable-org",
			tokenMetadataOrgID: nil,
			expectedOrgID:      "stable-org",
		},
		{
			name:               "legacy subscription retains token metadata",
			organizationID:     nil,
			tokenMetadataOrgID: "legacy-org",
			expectedOrgID:      "legacy-org",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subscriptionObject := createSubscription("test-sub", []string{"g1"}, nil, 10, defaultTestTokenRateLimit, "", "")
			spec, ok := subscriptionObject.Object["spec"].(map[string]any)
			if !ok {
				t.Fatalf("test subscription spec is not a map: %T", subscriptionObject.Object["spec"])
			}
			if test.organizationID != nil {
				spec["organization"] = map[string]any{"id": test.organizationID}
			}
			if test.tokenMetadataOrgID != nil {
				spec["tokenMetadata"] = map[string]any{"organizationId": test.tokenMetadataOrgID}
			}

			selector := subscription.NewSelector(logger.New(false), &fakeLister{subscriptions: []*unstructured.Unstructured{subscriptionObject}}, nil, nil)
			response, err := selector.Select([]string{"g1"}, "", "", "")
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if response.OrganizationID != test.expectedOrgID {
				t.Fatalf("OrganizationID = %q, want %q", response.OrganizationID, test.expectedOrgID)
			}
		})
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
			selector := subscription.NewSelector(log, lister, nil, nil)

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
		selector := subscription.NewSelector(log, lister, nil, nil)

		_, err := selector.GetAllAccessible(nil, "")
		if err == nil {
			t.Error("expected error when both groups and username are empty")
		}
		if err.Error() != "either groups or username must be provided" {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("authorization lookup failure returns an error", func(t *testing.T) {
		lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
			createSubscription("sub1", []string{"g1"}, nil, 1, defaultTestTokenRateLimit, "", ""),
		}}
		selector := subscription.NewSelector(log, lister, nil, &fakeAccessChecker{})

		_, err := selector.GetAllAccessible([]string{"g1"}, "")
		if err == nil {
			t.Fatal("expected authorization lookup error")
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
		sel := subscription.NewSelector(log, lister, nil, nil)
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
		sel := subscription.NewSelector(log, lister, nil, nil)
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
		sel := subscription.NewSelector(log, lister, nil, nil)
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
		sel := subscription.NewSelector(log, lister, nil, nil)
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

func TestSelector_RejectsDeletingSubscriptions(t *testing.T) {
	log := logger.New(false)
	deleting := createSubscriptionWithHealth("deleting-sub", []string{"g1"}, nil, 100, defaultTestTokenRateLimit, phaseActive, true, true)
	active := createSubscriptionWithHealth("active-sub", []string{"g1"}, nil, 10, defaultTestTokenRateLimit, phaseActive, true, false)

	t.Run("explicit selection", func(t *testing.T) {
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{deleting}}, nil, nil)

		_, err := selector.Select([]string{"g1"}, "", "deleting-sub", "")
		if err == nil {
			t.Fatal("expected deleting subscription to be rejected")
		}
	})

	t.Run("automatic selection", func(t *testing.T) {
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{deleting}}, nil, nil)

		_, err := selector.Select([]string{"g1"}, "", "", "")
		if err == nil {
			t.Fatal("expected deleting subscription to be excluded from automatic selection")
		}
	})

	t.Run("highest priority selection skips deleting subscription", func(t *testing.T) {
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{deleting, active}}, nil, nil)

		selected, err := selector.SelectHighestPriority([]string{"g1"}, "")
		if err != nil {
			t.Fatalf("SelectHighestPriority: %v", err)
		}
		if selected.Name != "active-sub" {
			t.Fatalf("selected subscription = %q, want active-sub", selected.Name)
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
			name:             "Failed subscription with Ready=False - rejected for API key creation",
			subscription:     createSubscriptionWithHealth("failed-sub", []string{"g1"}, nil, 10, 1000, phaseFailed, false, false),
			expectedPhase:    phaseFailed,
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      true, // Failed subscriptions rejected to prevent key spam
		},
		{
			name:             "Pending subscription with Ready=False - allowed for API key creation",
			subscription:     createSubscriptionWithHealth("pending-sub", []string{"g1"}, nil, 10, 1000, phasePending, false, false),
			expectedPhase:    phasePending,
			expectedReady:    false,
			expectedDeleting: false,
			expectError:      false, // Pending subscriptions allowed (optimistic - might become Active)
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
			expectError:      true, // Deleting subscriptions cannot receive new API keys
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
			selector := subscription.NewSelector(log, lister, nil, nil)

			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
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
	selector := subscription.NewSelector(log, lister, nil, nil)

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

func TestSelector_DegradedSubscriptionTRLPFiltering(t *testing.T) {
	log := logger.Production()

	tests := []struct {
		name                string
		subscription        *unstructured.Unstructured
		requestedModel      string
		expectError         bool
		expectedErrorReason string
	}{
		{
			name: "Degraded subscription with TRLP not ready - blocks inference",
			subscription: createSubscriptionWithTRLPStatus("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
			}, []map[string]any{
				{
					"model":     "model-a",
					"name":      "maas-trlp-model-a",
					"namespace": "ns",
					"ready":     false,
					"reason":    "NotAccepted",
					"message":   "status not available",
				},
			}),
			requestedModel:      "ns/model-a",
			expectError:         true,
			expectedErrorReason: "RateLimitNotEnforced",
		},
		{
			name: "Degraded subscription with all TRLPs ready - allows inference (partial model failure)",
			subscription: createSubscriptionWithTRLPStatus("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
				{
					"name":      "model-b",
					"namespace": "ns",
					"ready":     false,
					"reason":    "NotFound",
					"message":   "model not found",
				},
			}, []map[string]any{
				{
					"model":     "model-a",
					"name":      "maas-trlp-model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Accepted",
				},
			}),
			requestedModel: "ns/model-a",
			expectError:    false,
		},
		{
			name: "Active subscription - TRLP status doesn't matter",
			subscription: createSubscriptionWithTRLPStatus("active-sub", []string{"g1"}, phaseActive, []map[string]any{
				{
					"name":      "model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
			}, []map[string]any{
				{
					"model":     "model-a",
					"name":      "maas-trlp-model-a",
					"namespace": "ns",
					"ready":     false,
					"reason":    "NotAccepted",
				},
			}),
			requestedModel: "ns/model-a",
			expectError:    false,
		},
		{
			name: "Degraded subscription with multiple TRLPs - requested model TRLP ready allows inference",
			subscription: createSubscriptionWithTRLPStatus("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
				{
					"name":      "model-b",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
			}, []map[string]any{
				{
					"model":     "model-a",
					"name":      "maas-trlp-model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Accepted",
				},
				{
					"model":     "model-b",
					"name":      "maas-trlp-model-b",
					"namespace": "ns",
					"ready":     false,
					"reason":    "NotAccepted",
					"message":   "policy not accepted",
				},
			}),
			requestedModel: "ns/model-a",
			expectError:    false,
		},
		{
			name: "Degraded subscription with multiple TRLPs - requested model TRLP not ready blocks inference",
			subscription: createSubscriptionWithTRLPStatus("degraded-sub", []string{"g1"}, phaseDegraded, []map[string]any{
				{
					"name":      "model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
				{
					"name":      "model-b",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Valid",
				},
			}, []map[string]any{
				{
					"model":     "model-a",
					"name":      "maas-trlp-model-a",
					"namespace": "ns",
					"ready":     true,
					"reason":    "Accepted",
				},
				{
					"model":     "model-b",
					"name":      "maas-trlp-model-b",
					"namespace": "ns",
					"ready":     false,
					"reason":    "NotAccepted",
					"message":   "policy not accepted",
				},
			}),
			requestedModel:      "ns/model-b",
			expectError:         true,
			expectedErrorReason: "RateLimitNotEnforced",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: []*unstructured.Unstructured{tt.subscription}}
			selector := subscription.NewSelector(log, lister, nil, nil)

			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
			result, err := selector.Select([]string{"g1"}, "", "", tt.requestedModel)

			if tt.expectError {
				if err == nil {
					t.Fatalf("Expected error but got none")
				}
				var modelUnhealthyErr *subscription.ModelUnhealthyError
				if !errors.As(err, &modelUnhealthyErr) {
					t.Fatalf("Expected ModelUnhealthyError, got %T: %v", err, err)
				}
				if tt.expectedErrorReason != "" && modelUnhealthyErr.Reason != tt.expectedErrorReason {
					t.Fatalf("Expected error reason %q, got %q", tt.expectedErrorReason, modelUnhealthyErr.Reason)
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

// createSubscriptionWithTRLPStatus creates a test subscription with model and TRLP status.
func createSubscriptionWithTRLPStatus(name string, groups []string, phase string, modelStatuses []map[string]any, trlpStatuses []map[string]any) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	// Convert []map[string]any to []any for k8s deep copy compatibility
	modelStatusesAny := make([]any, len(modelStatuses))
	for i, status := range modelStatuses {
		modelStatusesAny[i] = status
	}

	trlpStatusesAny := make([]any, len(trlpStatuses))
	for i, status := range trlpStatuses {
		trlpStatusesAny[i] = status
	}

	// Build modelRefs from modelStatuses
	modelRefs := make([]any, 0, len(modelStatuses))
	for _, status := range modelStatuses {
		modelName, _ := status["name"].(string)
		modelNamespace, _ := status["namespace"].(string)
		modelRefs = append(modelRefs, map[string]any{
			"name":      modelName,
			"namespace": modelNamespace,
			"tokenRateLimits": []any{
				map[string]any{
					"limit":  int64(100),
					"window": "1m",
				},
			},
		})
	}

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"owner": map[string]any{
					"groups": groupsSlice,
				},
				"priority":  int64(10),
				"modelRefs": modelRefs,
			},
			"status": map[string]any{
				"phase": phase,
				"conditions": []any{
					map[string]any{
						"type":    "Ready",
						"status":  "True",
						"reason":  phase,
						"message": "test",
					},
				},
				"modelRefStatuses":       modelStatusesAny,
				"tokenRateLimitStatuses": trlpStatusesAny,
			},
		},
	}
	return obj
}

func createMaaSModelRef(name, namespace, kind string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSModelRef",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"modelRef": map[string]any{
					"kind": kind,
					"name": name,
				},
			},
		},
	}
}

func TestEnrichModelRefsSource(t *testing.T) {
	tests := []struct {
		name           string
		modelRefKind   string
		expectedSource string
	}{
		{
			name:           "ExternalModel kind maps to external",
			modelRefKind:   "ExternalModel",
			expectedSource: "external",
		},
		{
			name:           "LLMInferenceService kind maps to internal",
			modelRefKind:   "LLMInferenceService",
			expectedSource: "internal",
		},
		{
			name:           "empty kind leaves source empty",
			modelRefKind:   "",
			expectedSource: "",
		},
		{
			name:           "unknown kind leaves source empty",
			modelRefKind:   "SomeNewKind",
			expectedSource: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := logger.New(false)

			sub := createSubscription("test-sub", []string{"g1"}, nil, 1, defaultTestTokenRateLimit, "", "")

			modelRef := createMaaSModelRef("test-model", "", tt.modelRefKind)
			modelLister := &fakeModelLister{items: []*unstructured.Unstructured{modelRef}}

			lister := &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}
			selector := subscription.NewSelector(log, lister, modelLister, nil)

			accessible, err := selector.GetAllAccessible([]string{"g1"}, "")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if len(accessible) == 0 {
				t.Fatal("Expected at least one accessible subscription")
			}

			found := false
			for _, ref := range accessible[0].ModelRefs {
				if ref.Name == "test-model" {
					found = true
					if ref.Source != tt.expectedSource {
						t.Errorf("Expected source %q, got %q", tt.expectedSource, ref.Source)
					}
				}
			}
			if !found {
				t.Fatal("test-model not found in model refs")
			}
		})
	}
}

// fakeAccessChecker implements subscription.ModelAccessChecker for testing.
type fakeAccessChecker struct {
	authorized map[authpolicy.ModelKey]bool
	err        error
	gotGroups  []string
	gotUser    string
	calls      int
}

func (f *fakeAccessChecker) AuthorizedModels(groups []string, username string) (map[authpolicy.ModelKey]bool, error) {
	f.calls++
	f.gotGroups = groups
	f.gotUser = username
	return f.authorized, f.err
}

// createSubscriptionWithModelRefs creates a subscription with custom model refs (each with name and namespace).
func createSubscriptionWithModelRefs(subName string, groups []string, modelRefs []map[string]any) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	modelRefsSlice := make([]any, len(modelRefs))
	for i, ref := range modelRefs {
		modelRefsSlice[i] = ref
	}

	spec := map[string]any{
		"owner": map[string]any{
			"groups": groupsSlice,
		},
		"priority":  int64(10),
		"modelRefs": modelRefsSlice,
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      subName,
				"namespace": "test-ns",
			},
			"spec": spec,
			"status": map[string]any{
				"phase": phaseActive,
				"conditions": []any{
					map[string]any{
						"type":   "Ready",
						"status": "True",
					},
				},
			},
		},
	}
}

func createUserSubscriptionWithModelRefs(subName, username string, modelRefs []map[string]any) *unstructured.Unstructured {
	sub := createSubscriptionWithModelRefs(subName, nil, modelRefs)
	_ = unstructured.SetNestedStringSlice(sub.Object, []string{username}, "spec", "owner", "users")
	return sub
}

type selectAccessAllowedTestCase struct {
	name                  string
	subscriptions         []*unstructured.Unstructured
	groups                []string
	username              string
	requestedSubscription string
	requestedModel        string
	accessChecker         subscription.ModelAccessChecker
	wantAccessAllowed     bool
	wantError             bool
	wantAccessDenied      bool
	wantModelNotInSub     bool
}

func TestSelect_AccessAllowed(t *testing.T) {
	log := logger.New(false)
	for _, tt := range selectAccessAllowedTestCases() {
		t.Run(tt.name, func(t *testing.T) {
			runSelectAccessAllowedTest(t, log, tt)
		})
	}
}

func selectAccessAllowedTestCases() []selectAccessAllowedTestCase {
	return []selectAccessAllowedTestCase{
		{
			name: "authorized model returns AccessAllowed true",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:         []string{"g1"},
			requestedModel: "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns1", Name: "model-a"}: true,
				},
			},
			wantAccessAllowed: true,
		},
		{
			name: "unauthorized model returns AccessDeniedError",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:         []string{"g1"},
			requestedModel: "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns2", Name: "model-b"}: true,
				},
			},
			wantError:        true,
			wantAccessDenied: true,
		},
		{
			name: "nil access checker with model denies (fail-closed)",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:            []string{"g1"},
			requestedModel:    "ns1/model-a",
			accessChecker:     nil,
			wantAccessAllowed: false,
		},
		{
			name: "nil access checker without model allows",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:            []string{"g1"},
			requestedModel:    "",
			accessChecker:     nil,
			wantAccessAllowed: true,
		},
		{
			name: "empty requestedModel returns AccessAllowed true",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:         []string{"g1"},
			requestedModel: "",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{},
			},
			wantAccessAllowed: true,
		},
		{
			name: "explicit subscription with authorized model",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "sub1",
			requestedModel:        "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns1", Name: "model-a"}: true,
				},
			},
			wantAccessAllowed: true,
		},
		{
			name: "explicit subscription with unauthorized model",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "sub1",
			requestedModel:        "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{},
			},
			wantAccessAllowed: false,
		},
		{
			name: "qualified subscription with authorized model",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "test-ns/sub1",
			requestedModel:        "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns1", Name: "model-a"}: true,
				},
			},
			wantAccessAllowed: true,
		},
		{
			name: "explicit bare subscription with auth policy does not leak missing model",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "sub1",
			requestedModel:        "ns1/missing-model",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns1", Name: "model-a"}: true,
				},
			},
			wantAccessAllowed: false,
		},
		{
			name: "explicit bare subscription without auth policy returns model not in subscription",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "sub1",
			requestedModel:        "ns1/missing-model",
			wantError:             true,
			wantModelNotInSub:     true,
		},
		{
			name: "explicit qualified subscription with auth policy does not leak missing model",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "test-ns/sub1",
			requestedModel:        "ns1/missing-model",
			accessChecker: &fakeAccessChecker{
				authorized: map[authpolicy.ModelKey]bool{
					{Namespace: "ns1", Name: "model-a"}: true,
				},
			},
			wantAccessAllowed: false,
		},
		{
			name: "explicit qualified subscription without auth policy returns model not in subscription",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "test-ns/sub1",
			requestedModel:        "ns1/missing-model",
			wantError:             true,
			wantModelNotInSub:     true,
		},
		{
			name: "explicit subscription without auth policy returns model not in subscription",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:                []string{"g1"},
			requestedSubscription: "sub1",
			requestedModel:        "ns1/missing-model",
			accessChecker:         nil,
			wantError:             true,
			wantModelNotInSub:     true,
		},
		{
			name: "nil authorized set returns checker error",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
				}),
			},
			groups:         []string{"g1"},
			requestedModel: "ns1/model-a",
			accessChecker: &fakeAccessChecker{
				authorized: nil,
			},
			wantError: true,
		},
	}
}

func runSelectAccessAllowedTest(t *testing.T, log *logger.Logger, tt selectAccessAllowedTestCase) {
	t.Helper()
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: tt.subscriptions}, nil, tt.accessChecker)
	result, err := selector.Select(tt.groups, tt.username, tt.requestedSubscription, tt.requestedModel)
	if tt.wantError {
		assertSelectAccessAllowedError(t, err, tt)
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccessAllowed != tt.wantAccessAllowed {
		t.Errorf("AccessAllowed = %v, want %v", result.AccessAllowed, tt.wantAccessAllowed)
	}
}

func assertSelectAccessAllowedError(t *testing.T, err error, tt selectAccessAllowedTestCase) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if tt.wantAccessDenied {
		var denied *subscription.AccessDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected AccessDeniedError, got %T: %v", err, err)
		}
		if denied.Subscription != "" {
			t.Errorf("AccessDeniedError leaks subscription identifier %q", denied.Subscription)
		}
		if tt.requestedModel != "" && strings.Contains(err.Error(), tt.requestedModel) {
			t.Errorf("AccessDeniedError leaks requested model %q", tt.requestedModel)
		}
	}
	if tt.wantModelNotInSub {
		var notInSub *subscription.ModelNotInSubscriptionError
		if !errors.As(err, &notInSub) {
			t.Fatalf("expected ModelNotInSubscriptionError, got %T: %v", err, err)
		}
	}
}

type listAccessibleForModelTestCase struct {
	name          string
	subscriptions []*unstructured.Unstructured
	accessChecker subscription.ModelAccessChecker
	groups        []string
	username      string
	modelID       string
	wantCount     int
	wantSubNames  []string
	wantModelRefs map[string][]string
	wantError     bool
}

func TestListAccessibleForModel_MultiNamespace(t *testing.T) {
	log := logger.New(false)

	tests := []listAccessibleForModelTestCase{
		{
			name: "single namespace match authorized",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-a", Name: "model-x"}: true,
			}},
			groups:       []string{"g1"},
			modelID:      "model-x",
			wantCount:    1,
			wantSubNames: []string{"sub1"},
		},
		{
			name: "single namespace match not authorized",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-b", Name: "model-x"}: true,
			}},
			groups:    []string{"g1"},
			modelID:   "model-x",
			wantCount: 0,
		},
		{
			name: "multi namespace - first not authorized but second is",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
					{"name": "model-x", "namespace": "tenant-b"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-b", Name: "model-x"}: true,
			}},
			groups:       []string{"g1"},
			modelID:      "model-x",
			wantCount:    1,
			wantSubNames: []string{"sub1"},
		},
		{
			name: "multi namespace - none authorized",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
					{"name": "model-x", "namespace": "tenant-b"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-c", Name: "model-x"}: true,
			}},
			groups:    []string{"g1"},
			modelID:   "model-x",
			wantCount: 0,
		},
		{
			name: "multi namespace - both authorized",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
					{"name": "model-x", "namespace": "tenant-b"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-a", Name: "model-x"}: true,
				{Namespace: "tenant-b", Name: "model-x"}: true,
			}},
			groups:       []string{"g1"},
			modelID:      "model-x",
			wantCount:    1,
			wantSubNames: []string{"sub1"},
		},
		{
			name: "no access checker - all namespaces pass",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
					{"name": "model-x", "namespace": "tenant-b"},
				}),
			},
			groups:       []string{"g1"},
			modelID:      "model-x",
			wantCount:    1,
			wantSubNames: []string{"sub1"},
		},
		{
			name: "access checker returns nil authorized set - deny",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: nil},
			groups:        []string{"g1"},
			modelID:       "model-x",
			wantCount:     0,
			wantError:     true,
		},
		{
			name: "no matching AuthPolicy returns no subscriptions",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{}},
			groups:        []string{"g1"},
			modelID:       "model-x",
			wantCount:     0,
		},
		{
			name: "model not in subscription",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "other-model", "namespace": "tenant-a"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-a", Name: "model-x"}: true,
			}},
			groups:    []string{"g1"},
			modelID:   "model-x",
			wantCount: 0,
		},
		{
			name: "multiple subscriptions - mixed authorization",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
				}),
				createSubscriptionWithModelRefs("sub2", []string{"g1"}, []map[string]any{
					{"name": "model-x", "namespace": "tenant-a"},
					{"name": "model-x", "namespace": "tenant-b"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-b", Name: "model-x"}: true,
			}},
			groups:       []string{"g1"},
			modelID:      "model-x",
			wantCount:    1,
			wantSubNames: []string{"sub2"},
			wantModelRefs: map[string][]string{
				"sub2": {"tenant-b/model-x"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runListAccessibleForModelTest(t, log, tt)
		})
	}
}

func runListAccessibleForModelTest(t *testing.T, log *logger.Logger, tt listAccessibleForModelTestCase) {
	t.Helper()
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: tt.subscriptions}, nil, tt.accessChecker)
	result, err := selector.ListAccessibleForModel(tt.username, tt.groups, tt.modelID)
	if tt.wantError {
		if err == nil {
			t.Fatal("expected authorization lookup error")
		}
		return
	}
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	assertAccessibleSubscriptions(t, result, tt)
}

func assertAccessibleSubscriptions(t *testing.T, result []subscription.SubscriptionInfo, tt listAccessibleForModelTestCase) {
	t.Helper()
	if len(result) != tt.wantCount {
		t.Errorf("Expected %d subscriptions, got %d", tt.wantCount, len(result))
	}
	assertSubscriptionNames(t, result, tt.wantSubNames)
	assertModelRefsBySubscription(t, result, tt.wantModelRefs)
}

func assertSubscriptionNames(t *testing.T, result []subscription.SubscriptionInfo, wantNames []string) {
	t.Helper()
	for i, wantName := range wantNames {
		if i >= len(result) {
			t.Errorf("Missing expected subscription %q at index %d", wantName, i)
			continue
		}
		if result[i].SubscriptionIDHeader != wantName {
			t.Errorf("Expected subscription %q at index %d, got %q", wantName, i, result[i].SubscriptionIDHeader)
		}
	}
}

func assertModelRefsBySubscription(t *testing.T, result []subscription.SubscriptionInfo, wantRefsBySubscription map[string][]string) {
	t.Helper()
	for subName, wantRefs := range wantRefsBySubscription {
		gotRefs := modelRefsForSubscription(result, subName)
		if !slices.Equal(gotRefs, wantRefs) {
			t.Errorf("model refs for %q = %v, want %v", subName, gotRefs, wantRefs)
		}
	}
}

func modelRefsForSubscription(result []subscription.SubscriptionInfo, subName string) []string {
	var gotRefs []string
	for _, info := range result {
		if info.SubscriptionIDHeader != subName {
			continue
		}
		for _, ref := range info.ModelRefs {
			gotRefs = append(gotRefs, ref.Namespace+"/"+ref.Name)
		}
	}
	return gotRefs
}

type selectFiltersModelRefsTestCase struct {
	name               string
	subscriptions      []*unstructured.Unstructured
	accessChecker      *fakeAccessChecker
	groups             []string
	username           string
	requestedSub       string
	requestedModel     string
	wantModelRefs      []string
	wantAccessAllowed  bool
	expectError        bool
	expectAccessDenied bool
}

func TestSelect_FiltersModelRefsByAuthPolicy(t *testing.T) {
	runSelectFiltersModelRefsCases(t, []selectFiltersModelRefsTestCase{
		{
			name: "Select auto filters modelRefs by AuthPolicy",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
					{"name": "model-c", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: true,
		},
		{
			name: "Select returns no modelRefs when authorization grants none",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker:     &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{}},
			groups:            []string{"g1"},
			wantModelRefs:     []string{},
			wantAccessAllowed: true,
		},
		{
			name: "Select filters same model name by namespace",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "tenant-a"},
					{"name": "model-a", "namespace": "tenant-b"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "tenant-b", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			wantModelRefs:     []string{"tenant-b/model-a"},
			wantAccessAllowed: true,
		},
		{
			name: "Select passes username identity to access checker",
			subscriptions: []*unstructured.Unstructured{
				createUserSubscriptionWithModelRefs("sub1", "alice", []map[string]any{
					{"name": "model-a", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			username:          "alice",
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: true,
		},
		{
			name: "Select without access checker returns all models",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			groups:            []string{"g1"},
			wantModelRefs:     []string{"ns/model-a", "ns/model-b"},
			wantAccessAllowed: true,
		},
		{
			name: "Select auto rejects unauthorized requestedModel",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:             []string{"g1"},
			requestedModel:     "ns/model-b",
			expectError:        true,
			expectAccessDenied: true,
		},
		{
			name: "Select auto allows authorized requestedModel",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedModel:    "ns/model-a",
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: true,
		},
		{
			name: "Select with nil authorizedSet (checker error) fails closed",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{},
			groups:        []string{"g1"},
			expectError:   true,
		},
		{
			name: "Select auto with nil authorizedSet rejects requestedModel",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
				}),
			},
			accessChecker:  &fakeAccessChecker{},
			groups:         []string{"g1"},
			requestedModel: "ns/model-a",
			expectError:    true,
		},
		{
			name: "Select auto two subscriptions unauthorized model returns AccessDeniedError",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
				}),
				createSubscriptionWithModelRefs("sub2", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-b"}: true,
			}},
			groups:             []string{"g1"},
			requestedModel:     "ns/model-a",
			expectError:        true,
			expectAccessDenied: true,
		},
	})
}

func TestSelectExplicit_FiltersModelRefsByAuthPolicy(t *testing.T) {
	runSelectFiltersModelRefsCases(t, []selectFiltersModelRefsTestCase{
		{
			name: "Select explicit subscription filters modelRefs by AuthPolicy",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-b"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "sub1",
			wantModelRefs:     []string{"ns/model-b"},
			wantAccessAllowed: true,
		},
		{
			name: "Select explicit qualified subscription filters modelRefs",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "test-ns/sub1",
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: true,
		},
		{
			name: "Select explicit bare subscription hides missing model membership",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
					{"name": "model-b", "namespace": "ns2"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns1", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "sub1",
			requestedModel:    "ns2/missing-model",
			wantModelRefs:     []string{"ns1/model-a"},
			wantAccessAllowed: false,
		},
		{
			name: "Select explicit qualified subscription hides missing model membership",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns1"},
					{"name": "model-b", "namespace": "ns2"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns1", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "test-ns/sub1",
			requestedModel:    "ns2/missing-model",
			wantModelRefs:     []string{"ns1/model-a"},
			wantAccessAllowed: false,
		},
		{
			name: "Select explicit subscription with unauthorized model returns AccessAllowed false",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "sub1",
			requestedModel:    "ns/model-b",
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: false,
		},
		{
			name: "Select skips health checks for unauthorized degraded model",
			subscriptions: []*unstructured.Unstructured{
				func() *unstructured.Unstructured {
					sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
						{
							"name":      "model-a",
							"namespace": "ns",
							"tokenRateLimits": []any{
								map[string]any{"limit": int64(1000), "window": "1m"},
							},
						},
						{"name": "model-b", "namespace": "ns"},
					})
					sub.Object["status"] = map[string]any{"phase": phaseDegraded}
					return sub
				}(),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-b"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "sub1",
			requestedModel:    "ns/model-a",
			wantModelRefs:     []string{"ns/model-b"},
			wantAccessAllowed: false,
		},
		{
			name: "Select qualified subscription with unauthorized model returns AccessAllowed false",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
			}},
			groups:            []string{"g1"},
			requestedSub:      "test-ns/sub1",
			requestedModel:    "ns/model-b",
			wantModelRefs:     []string{"ns/model-a"},
			wantAccessAllowed: false,
		},
	})
}

func runSelectFiltersModelRefsCases(t *testing.T, tests []selectFiltersModelRefsTestCase) {
	t.Helper()
	log := logger.New(false)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runSelectFiltersModelRefsTest(t, log, tt)
		})
	}
}

func runSelectFiltersModelRefsTest(t *testing.T, log *logger.Logger, tt selectFiltersModelRefsTestCase) {
	t.Helper()
	var accessChecker subscription.ModelAccessChecker
	if tt.accessChecker != nil {
		accessChecker = tt.accessChecker
	}
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: tt.subscriptions}, nil, accessChecker)

	//nolint:unqueryvet,nolintlint // False positive - not a SQL query
	result, err := selector.Select(tt.groups, tt.username, tt.requestedSub, tt.requestedModel)
	assertAccessCheckerCall(t, tt)
	if tt.expectError {
		requireAccessDeniedError(t, err, tt.expectAccessDenied)
		return
	}
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}

	assertModelRefs(t, result.ModelRefs, tt.wantModelRefs)
	if result.AccessAllowed != tt.wantAccessAllowed {
		t.Errorf("AccessAllowed = %v, want %v", result.AccessAllowed, tt.wantAccessAllowed)
	}
}

func assertAccessCheckerCall(t *testing.T, tt selectFiltersModelRefsTestCase) {
	t.Helper()
	if tt.accessChecker == nil {
		return
	}
	if tt.accessChecker.calls != 1 {
		t.Errorf("AuthorizedModels calls = %d, want 1", tt.accessChecker.calls)
	}
	if !slices.Equal(tt.accessChecker.gotGroups, tt.groups) {
		t.Errorf("AuthorizedModels groups = %v, want %v", tt.accessChecker.gotGroups, tt.groups)
	}
	if tt.accessChecker.gotUser != tt.username {
		t.Errorf("AuthorizedModels username = %q, want %q", tt.accessChecker.gotUser, tt.username)
	}
}

func requireAccessDeniedError(t *testing.T, err error, expectAccessDenied bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if expectAccessDenied {
		var accessDenied *subscription.AccessDeniedError
		if !errors.As(err, &accessDenied) {
			t.Fatalf("expected AccessDeniedError, got %T: %v", err, err)
		}
	}
}

func assertModelRefs(t *testing.T, refs []subscription.ModelRefInfo, wantRefs []string) {
	t.Helper()
	gotRefs := make([]string, len(refs))
	for i, ref := range refs {
		gotRefs[i] = ref.Namespace + "/" + ref.Name
	}
	if len(gotRefs) != len(wantRefs) {
		t.Fatalf("expected %d model refs %v, got %d: %v", len(wantRefs), wantRefs, len(gotRefs), gotRefs)
	}
	for i, want := range wantRefs {
		if gotRefs[i] != want {
			t.Errorf("model ref[%d] = %q, want %q", i, gotRefs[i], want)
		}
	}
}

func TestSelectHighestPriority_FiltersModelRefsByAuthPolicy(t *testing.T) {
	log := logger.New(false)

	tests := []struct {
		name          string
		subscriptions []*unstructured.Unstructured
		accessChecker *fakeAccessChecker
		groups        []string
		wantModelRefs []string
		wantError     bool
	}{
		{
			name: "filters modelRefs by AuthPolicy",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
					{"name": "model-c", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{
				{Namespace: "ns", Name: "model-a"}: true,
				{Namespace: "ns", Name: "model-c"}: true,
			}},
			groups:        []string{"g1"},
			wantModelRefs: []string{"ns/model-a", "ns/model-c"},
		},
		{
			name: "without access checker returns all models",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			groups:        []string{"g1"},
			wantModelRefs: []string{"ns/model-a", "ns/model-b"},
		},
		{
			name: "nil authorizedSet (checker error) fails closed",
			subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
					{"name": "model-a", "namespace": "ns"},
					{"name": "model-b", "namespace": "ns"},
				}),
			},
			accessChecker: &fakeAccessChecker{},
			groups:        []string{"g1"},
			wantError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: tt.subscriptions}

			var accessChecker subscription.ModelAccessChecker
			if tt.accessChecker != nil {
				accessChecker = tt.accessChecker
			}
			selector := subscription.NewSelector(log, lister, nil, accessChecker)
			result, err := selector.SelectHighestPriority(tt.groups, "")
			if tt.wantError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectHighestPriority() error = %v", err)
			}

			assertModelRefs(t, result.ModelRefs, tt.wantModelRefs)
		})
	}
}

func TestSelector_ResolvedModelFromAlias(t *testing.T) {
	cases := []struct {
		name         string
		alias        string
		canonicalRef string
		subName      string
		modelName    string
		modelNS      string
		modelKind    string
	}{
		{
			name:         "publisher alias (LLMInferenceService)",
			alias:        "publishers/llm/models/facebook/opt-125m",
			canonicalRef: "llm/facebook-opt-125m-simulated",
			subName:      "simulator-subscription",
			modelName:    "facebook-opt-125m-simulated",
			modelNS:      "llm",
			modelKind:    "LLMInferenceService",
		},
		{
			name:         "external model alias (ExternalModel)",
			alias:        "gpt-3.5-turbo",
			canonicalRef: "llm/e2e-external-model",
			subName:      "external-subscription",
			modelName:    "e2e-external-model",
			modelNS:      "llm",
			modelKind:    "ExternalModel",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := logger.New(false)

			sub := createSubscriptionWithModelRefs(tc.subName, []string{"g1"}, []map[string]any{
				{"name": tc.modelName, "namespace": tc.modelNS},
			})

			modelRef := createMaaSModelRef(tc.modelName, tc.modelNS, tc.modelKind)
			if err := unstructured.SetNestedField(modelRef.Object, tc.alias, "status", "resolvedModelAlias"); err != nil {
				t.Fatalf("SetNestedField: %v", err)
			}

			selector := subscription.NewSelector(
				log,
				&fakeLister{subscriptions: []*unstructured.Unstructured{sub}},
				&fakeModelLister{items: []*unstructured.Unstructured{modelRef}},
				nil,
			)

			t.Run("alias resolves to MaaSModelRef identity", func(t *testing.T) {
				//nolint:unqueryvet,nolintlint // False positive - not a SQL query
				result, err := selector.Select([]string{"g1"}, "", "", tc.alias)
				if err != nil {
					t.Fatalf("Select: %v", err)
				}
				if result.ResolvedModel != tc.canonicalRef {
					t.Errorf("ResolvedModel = %q, want %q", result.ResolvedModel, tc.canonicalRef)
				}
			})

			t.Run("path-style identity is returned unchanged", func(t *testing.T) {
				//nolint:unqueryvet,nolintlint // False positive - not a SQL query
				result, err := selector.Select([]string{"g1"}, "", "", tc.canonicalRef)
				if err != nil {
					t.Fatalf("Select: %v", err)
				}
				if result.ResolvedModel != tc.canonicalRef {
					t.Errorf("ResolvedModel = %q, want %q", result.ResolvedModel, tc.canonicalRef)
				}
			})
		})
	}
}

// A subscription may list several modelRefs sharing one client-facing name
// (per-tenant modelRefs for a shared model, or distinct models with one
// client-facing name) while the caller's authorized set covers only some of
// them. findModelRef returns the first alias match, so authorization must
// not consider unauthorized refs: an unauthorized ref listed first must not
// deny a request that a later, authorized ref serves.
func TestSelect_SharedAliasAuthorizationOrdering(t *testing.T) {
	log := logger.New(false)
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
		{"name": "model-a", "namespace": "ns1"},
		{"name": "model-b", "namespace": "ns2"},
	})
	aliased := func(name, ns string) *unstructured.Unstructured {
		ref := createMaaSModelRef(name, ns, "ExternalModel")
		if err := unstructured.SetNestedField(ref.Object, "client-name", "status", "resolvedModelAlias"); err != nil {
			t.Fatalf("SetNestedField: %v", err)
		}
		return ref
	}
	modelLister := &fakeModelLister{items: []*unstructured.Unstructured{
		aliased("model-a", "ns1"),
		aliased("model-b", "ns2"),
	}}
	accessChecker := &fakeAccessChecker{
		authorized: map[authpolicy.ModelKey]bool{
			{Namespace: "ns2", Name: "model-b"}: true,
		},
	}
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, modelLister, accessChecker)

	t.Run("auto selection allows the authorized carrier", func(t *testing.T) {
		result, err := selector.Select([]string{"g1"}, "", "", "client-name")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if !result.AccessAllowed {
			t.Error("AccessAllowed = false, want true when a later same-alias ref is authorized")
		}
	})

	t.Run("explicit selection allows the authorized carrier", func(t *testing.T) {
		result, err := selector.Select([]string{"g1"}, "", "sub1", "client-name")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if !result.AccessAllowed {
			t.Error("AccessAllowed = false, want true when a later same-alias ref is authorized")
		}
	})

	t.Run("no authorized carrier denies", func(t *testing.T) {
		none := &fakeAccessChecker{authorized: map[authpolicy.ModelKey]bool{}}
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, modelLister, none)
		_, err := selector.Select([]string{"g1"}, "", "", "client-name")
		var denied *subscription.AccessDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected AccessDeniedError, got %T: %v", err, err)
		}
	})
}

// An exact namespace/name request must not be authorized through a different
// reference that merely carries the same client-facing name: the exact
// reference identifies the routed model, and its authorization result is the
// answer even when an alias carrier in the same namespace is authorized.
func TestSelect_ExactRefNotSubstitutedByAuthorizedAliasCarrier(t *testing.T) {
	log := logger.New(false)
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
		{"name": "client-name", "namespace": "tenant-b"},
		{"name": "modelref-b", "namespace": "tenant-b"},
	})
	aliased := func(name, ns string) *unstructured.Unstructured {
		ref := createMaaSModelRef(name, ns, "ExternalModel")
		if err := unstructured.SetNestedField(ref.Object, "client-name", "status", "resolvedModelAlias"); err != nil {
			t.Fatalf("SetNestedField: %v", err)
		}
		return ref
	}
	modelLister := &fakeModelLister{items: []*unstructured.Unstructured{
		aliased("client-name", "tenant-b"),
		aliased("modelref-b", "tenant-b"),
	}}
	accessChecker := &fakeAccessChecker{
		authorized: map[authpolicy.ModelKey]bool{
			{Namespace: "tenant-b", Name: "modelref-b"}: true,
		},
	}
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, modelLister, accessChecker)

	t.Run("explicit selection denies the unauthorized exact reference", func(t *testing.T) {
		result, err := selector.Select([]string{"g1"}, "", "sub1", "tenant-b/client-name")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if result.AccessAllowed {
			t.Error("AccessAllowed = true, want false when the exact reference is unauthorized")
		}
	})

	t.Run("auto selection denies the unauthorized exact reference", func(t *testing.T) {
		_, err := selector.Select([]string{"g1"}, "", "", "tenant-b/client-name")
		var denied *subscription.AccessDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("expected AccessDeniedError, got %T: %v", err, err)
		}
	})
}

// The auto-selection pre-filter must admit a subscription whose refs share a
// client-facing name in one namespace when the authorized subset resolves the
// ambiguity, instead of dropping it with NoSubscriptionError before the
// authorization filter can run.
func TestSelect_AmbiguousAliasAuthorizedSubsetAutoSelection(t *testing.T) {
	log := logger.New(false)
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
		{"name": "modelref-a", "namespace": "tenant-a"},
		{"name": "modelref-b", "namespace": "tenant-a"},
	})
	aliased := func(name, ns string) *unstructured.Unstructured {
		ref := createMaaSModelRef(name, ns, "ExternalModel")
		if err := unstructured.SetNestedField(ref.Object, "client-name", "status", "resolvedModelAlias"); err != nil {
			t.Fatalf("SetNestedField: %v", err)
		}
		return ref
	}
	modelLister := &fakeModelLister{items: []*unstructured.Unstructured{
		aliased("modelref-a", "tenant-a"),
		aliased("modelref-b", "tenant-a"),
	}}
	accessChecker := &fakeAccessChecker{
		authorized: map[authpolicy.ModelKey]bool{
			{Namespace: "tenant-a", Name: "modelref-b"}: true,
		},
	}
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, modelLister, accessChecker)

	result, err := selector.Select([]string{"g1"}, "", "", "tenant-a/client-name")
	if err != nil {
		t.Fatalf("Select: %v, want selection via the authorized carrier", err)
	}
	if !result.AccessAllowed {
		t.Error("AccessAllowed = false, want true when exactly one ambiguous carrier is authorized")
	}
}

func TestSelect_RateLimitIDOnlyForShortIdentity(t *testing.T) {
	log := logger.New(false)
	requestedModel := "ns1/model-a"

	legacy := createSubscriptionWithModelRefs("legacy-sub", []string{"g1"}, []map[string]any{
		{"name": "model-a", "namespace": "ns1"},
	})
	fresh := createSubscriptionWithModelRefs("fresh-sub", []string{"g1"}, []map[string]any{
		{"name": "model-a", "namespace": "ns1"},
	})
	if status, ok := fresh.Object["status"].(map[string]any); ok {
		status["rateLimitIdentity"] = constant.RateLimitIdentityShort
	} else {
		fresh.Object["status"] = map[string]any{
			"rateLimitIdentity": constant.RateLimitIdentityShort,
		}
	}

	t.Run("legacy omits rateLimitId", func(t *testing.T) {
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{legacy}}, nil, nil)
		result, err := selector.Select([]string{"g1"}, "", "", requestedModel)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if result.RateLimitID != "" {
			t.Errorf("RateLimitID = %q, want empty for legacy subscription", result.RateLimitID)
		}
	})

	t.Run("short status returns rateLimitId", func(t *testing.T) {
		selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{fresh}}, nil, nil)
		result, err := selector.Select([]string{"g1"}, "", "", requestedModel)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		want := subscription.RateLimitIDFor("test-ns", "fresh-sub", requestedModel)
		if result.RateLimitID != want {
			t.Errorf("RateLimitID = %q, want %q", result.RateLimitID, want)
		}
	})
}

// TRLP limits are keyed on the modelref, not the requested identity: a
// request carrying a client-facing alias — bare or as a path pair's name
// segment — must resolve ResolvedModel (and with it RateLimitID) to the
// matched ref's namespace/name so the modelref's rate limits apply.
func TestSelect_RateLimitIDUsesMatchedRefOnAliasPath(t *testing.T) {
	log := logger.New(false)
	sub := createSubscriptionWithModelRefs("fresh-sub", []string{"g1"}, []map[string]any{
		{"name": "model-a", "namespace": "ns1"},
	})
	if status, ok := sub.Object["status"].(map[string]any); ok {
		status["rateLimitIdentity"] = constant.RateLimitIdentityShort
	} else {
		sub.Object["status"] = map[string]any{
			"rateLimitIdentity": constant.RateLimitIdentityShort,
		}
	}
	modelRef := createMaaSModelRef("model-a", "ns1", "ExternalModel")
	if err := unstructured.SetNestedField(modelRef.Object, "client-name", "status", "resolvedModelAlias"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	selector := subscription.NewSelector(
		log,
		&fakeLister{subscriptions: []*unstructured.Unstructured{sub}},
		&fakeModelLister{items: []*unstructured.Unstructured{modelRef}},
		nil,
	)

	wantModel := "ns1/model-a"
	wantRateLimitID := subscription.RateLimitIDFor("test-ns", "fresh-sub", wantModel)

	for _, requested := range []string{"ns1/client-name", "client-name"} {
		t.Run("request "+requested, func(t *testing.T) {
			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
			result, err := selector.Select([]string{"g1"}, "", "", requested)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if result.ResolvedModel != wantModel {
				t.Errorf("ResolvedModel = %q, want %q", result.ResolvedModel, wantModel)
			}
			if result.RateLimitID != wantRateLimitID {
				t.Errorf("RateLimitID = %q, want %q", result.RateLimitID, wantRateLimitID)
			}
		})
	}
	t.Run("bare alias with several carriers resolves to the matched ref", func(t *testing.T) {
		sharedSub := createSubscriptionWithModelRefs("fresh-sub", []string{"g1"}, []map[string]any{
			{"name": "model-a", "namespace": "ns1"},
			{"name": "model-b", "namespace": "ns2"},
		})
		if status, ok := sharedSub.Object["status"].(map[string]any); ok {
			status["rateLimitIdentity"] = constant.RateLimitIdentityShort
		} else {
			sharedSub.Object["status"] = map[string]any{
				"rateLimitIdentity": constant.RateLimitIdentityShort,
			}
		}
		aliased := func(name, ns string) *unstructured.Unstructured {
			ref := createMaaSModelRef(name, ns, "ExternalModel")
			if err := unstructured.SetNestedField(ref.Object, "client-name", "status", "resolvedModelAlias"); err != nil {
				t.Fatalf("SetNestedField: %v", err)
			}
			return ref
		}
		sharedSelector := subscription.NewSelector(
			log,
			&fakeLister{subscriptions: []*unstructured.Unstructured{sharedSub}},
			&fakeModelLister{items: []*unstructured.Unstructured{
				aliased("model-a", "ns1"),
				aliased("model-b", "ns2"),
			}},
			nil,
		)
		//nolint:unqueryvet,nolintlint // False positive - not a SQL query
		result, err := sharedSelector.Select([]string{"g1"}, "", "", "client-name")
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if result.ResolvedModel != wantModel {
			t.Errorf("ResolvedModel = %q, want %q", result.ResolvedModel, wantModel)
		}
		if result.RateLimitID != wantRateLimitID {
			t.Errorf("RateLimitID = %q, want %q", result.RateLimitID, wantRateLimitID)
		}
	})
}

// createSubscriptionWithFlowControl returns a subscription for models ns1/model-a and
// ns1/model-b with the given status.flowControlStatuses.
func createSubscriptionWithFlowControl(t *testing.T, statuses ...map[string]any) *unstructured.Unstructured {
	t.Helper()
	sub := createSubscriptionWithModelRefs("sub1", []string{"g1"}, []map[string]any{
		{"name": "model-a", "namespace": "ns1"},
		{"name": "model-b", "namespace": "ns1"},
	})
	rows := make([]any, len(statuses))
	for i, s := range statuses {
		rows[i] = s
	}
	if err := unstructured.SetNestedSlice(sub.Object, rows, "status", "flowControlStatuses"); err != nil {
		t.Fatalf("SetNestedSlice: %v", err)
	}
	return sub
}

func flowControlRow(model, objective, reason string) map[string]any {
	return map[string]any{"name": model, "namespace": "ns1", "objectiveName": objective, "reason": reason, "ready": true}
}

func TestSelect_FlowControlObjectiveAndFairnessID(t *testing.T) {
	log := logger.New(false)
	sub := createSubscriptionWithFlowControl(t,
		flowControlRow("model-a", "maas-tenant-sub1-pool-a-0123456789", "ObjectiveReconciled"),
		flowControlRow("model-b", "maas-tenant-sub1-pool-b-0123456789", "ObjectiveConflict"),
	)
	aliased := createMaaSModelRef("model-a", "ns1", "LLMInferenceService")
	if err := unstructured.SetNestedField(aliased.Object, "publishers/ns1/models/a", "status", "resolvedModelAlias"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}},
		&fakeModelLister{items: []*unstructured.Unstructured{aliased}}, nil, subscription.WithTenantName("acme"))

	tests := []struct {
		name          string
		model         string
		wantObjective string
	}{
		{name: "namespace/name", model: "ns1/model-a", wantObjective: "maas-tenant-sub1-pool-a-0123456789"},
		{name: "bare name", model: "model-a", wantObjective: "maas-tenant-sub1-pool-a-0123456789"},
		{name: "body-routed alias", model: "publishers/ns1/models/a", wantObjective: "maas-tenant-sub1-pool-a-0123456789"},
		{name: "objective held by a foreign object is not returned", model: "ns1/model-b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
			resp, err := selector.Select([]string{"g1"}, "", "", tc.model)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if resp.Objective != tc.wantObjective {
				t.Errorf("Objective = %q, want %q", resp.Objective, tc.wantObjective)
			}
			if resp.FairnessID != "acme" {
				t.Errorf("FairnessID = %q, want acme", resp.FairnessID)
			}
		})
	}
}

func TestSelect_FlowControlObjectiveMissing(t *testing.T) {
	log := logger.New(false)
	tests := []struct {
		name string
		sub  *unstructured.Unstructured
	}{
		{name: "no flowControlStatuses", sub: createSubscriptionWithFlowControl(t)},
		{name: "no row for the model", sub: createSubscriptionWithFlowControl(t, flowControlRow("model-b", "maas-b", "PriorityUnset"))},
		{name: "row without an objective name", sub: createSubscriptionWithFlowControl(t, flowControlRow("model-a", "", "NotApplicable"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selector := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{tc.sub}}, nil, nil)
			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
			resp, err := selector.Select([]string{"g1"}, "", "", "ns1/model-a")
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if resp.Objective != "" || resp.FairnessID != "" {
				t.Errorf("Objective = %q, FairnessID = %q; want both empty", resp.Objective, resp.FairnessID)
			}
		})
	}
}
