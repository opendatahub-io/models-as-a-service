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
	"regexp"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestDeletionTimestampSet(t *testing.T) {
	tests := []struct {
		name     string
		oldObj   client.Object
		newObj   client.Object
		expected bool
	}{
		{
			name: "deletion timestamp transitions from nil to non-nil",
			oldObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
			},
			newObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			},
			expected: true,
		},
		{
			name: "deletion timestamp already set",
			oldObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			},
			newObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			},
			expected: false,
		},
		{
			name: "no deletion timestamp",
			oldObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
			},
			newObj: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := event.UpdateEvent{
				ObjectOld: tt.oldObj,
				ObjectNew: tt.newObj,
			}
			got := deletionTimestampSet(e)
			if got != tt.expected {
				t.Errorf("deletionTimestampSet() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestTokenRateLimitWindowPattern validates the kubebuilder regex pattern applied to
// TokenRateLimit.Window (defined in maassubscription_types.go).
//
// Background: MaaSSubscription.tokenRateLimits[].window values are passed through
// verbatim into Kuadrant TokenRateLimitPolicy rates[].window. Kuadrant only accepts
// s (seconds), m (minutes), and h (hours) with short numeric segments. The previous
// pattern (^(\d+)(s|m|h|d)$) allowed d (days) and unbounded numbers, both of which
// Kuadrant rejects at TRLP apply time. The tightened pattern (^[1-9]\d{0,3}(s|m|h)$)
// ensures CRD admission catches invalid values before they reach the controller.
//
// Pattern breakdown:
//   - ^[1-9]    — first digit must be 1-9 (no leading zeros, no zero window)
//   - \d{0,3}   — up to 3 more digits (total 1-4 digits → range 1-9999)
//   - (s|m|h)   — only Kuadrant-compatible time units
//   - $         — no trailing characters
func TestTokenRateLimitWindowPattern(t *testing.T) {
	// This must stay in sync with the +kubebuilder:validation:Pattern marker on
	// TokenRateLimit.Window in maassubscription_types.go. If the marker changes,
	// update this constant and re-run the test to verify.
	windowPattern := regexp.MustCompile(`^[1-9]\d{0,3}(s|m|h)$`)

	tests := []struct {
		name  string
		value string
		valid bool
	}{
		// --- valid: each Kuadrant-accepted unit with typical values ---
		{"1 second", "1s", true},
		{"1 minute", "1m", true},
		{"1 hour", "1h", true},
		{"30 seconds", "30s", true},
		{"5 minutes", "5m", true},
		{"24 hours", "24h", true}, // common replacement for "1d"

		// --- valid: numeric boundary values (1-9999) ---
		{"max 4-digit value", "9999h", true}, // upper boundary
		{"3-digit value", "100m", true},
		{"2-digit value", "10s", true},
		{"single digit", "9s", true}, // lower boundary (besides 1)

		// --- invalid: days unit ---
		// Previously allowed by the old pattern. Kuadrant does not support "d";
		// users should convert to hours (e.g. "1d" → "24h", "7d" → "168h").
		{"days not allowed", "1d", false},
		{"7 days not allowed", "7d", false},
		{"30 days not allowed", "30d", false},

		// --- invalid: leading zero ---
		// Leading zeros produce ambiguous values and are not valid Kuadrant input.
		{"leading zero", "01m", false},
		{"leading zero hours", "024h", false},

		// --- invalid: zero value ---
		// A zero-length window is meaningless for rate limiting.
		{"zero seconds", "0s", false},
		{"zero minutes", "0m", false},
		{"zero hours", "0h", false},

		// --- invalid: exceeds 4-digit cap ---
		// Kuadrant rejects oversized numeric segments. The pattern caps at 9999.
		{"5-digit value", "10000s", false},
		{"6-digit value", "100000m", false},

		// --- invalid: unsupported units ---
		// Kuadrant does not accept milliseconds, and the pattern is case-sensitive.
		{"milliseconds not allowed", "100ms", false},
		{"uppercase day", "1D", false},
		{"weeks not allowed", "1w", false},

		// --- invalid: malformed input ---
		// Catch-all cases for input that doesn't match the expected format at all.
		{"no unit", "100", false},
		{"no number", "m", false},
		{"empty string", "", false},
		{"leading whitespace", " 1m", false},
		{"trailing whitespace", "1m ", false},
		{"decimal", "1.5h", false},
		{"negative", "-1m", false},
		{"go duration", "1h30m", false}, // compound durations are not supported
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := windowPattern.MatchString(tt.value)
			if got != tt.valid {
				t.Errorf("windowPattern.MatchString(%q) = %v, want %v", tt.value, got, tt.valid)
			}
		})
	}
}

func TestValidateCELValue(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		fieldName string
		wantErr   bool
	}{
		{
			name:      "valid value",
			value:     "valid-value",
			fieldName: "test",
			wantErr:   false,
		},
		{
			name:      "value with double quote",
			value:     `value"with"quote`,
			fieldName: "test",
			wantErr:   true,
		},
		{
			name:      "value with backslash",
			value:     `value\with\backslash`,
			fieldName: "test",
			wantErr:   true,
		},
		{
			name:      "value with both double quote and backslash",
			value:     `value"\mixed`,
			fieldName: "test",
			wantErr:   true,
		},
		{
			name:      "empty value",
			value:     "",
			fieldName: "test",
			wantErr:   false,
		},
		{
			name:      "single quotes are allowed (only double-quoted CEL literals are used)",
			value:     "value'with'quotes",
			fieldName: "test",
			wantErr:   false,
		},
		{
			name:      "newline is allowed (CEL strings handle these)",
			value:     "value\nwith\nnewlines",
			fieldName: "test",
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCELValue(tt.value, tt.fieldName)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCELValue() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFindAllSubscriptionsForModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		modelNamespace string
		modelName      string
		subscriptions  []*maasv1alpha1.MaaSSubscription
		wantCount      int
	}{
		{
			name:           "no subscriptions",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions:  nil,
			wantCount:      0,
		},
		{
			name:           "one matching subscription",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions: []*maasv1alpha1.MaaSSubscription{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 1,
		},
		{
			name:           "multiple matching subscriptions",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions: []*maasv1alpha1.MaaSSubscription{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub2", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 2,
		},
		{
			name:           "exclude subscriptions being deleted",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions: []*maasv1alpha1.MaaSSubscription{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "sub2",
						Namespace:         "sub-ns",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"test-finalizer"},
					},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []maasv1alpha1.MaaSSubscription
			for _, sub := range tt.subscriptions {
				objects = append(objects, *sub)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithLists(&maasv1alpha1.MaaSSubscriptionList{Items: objects}).
				WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
				Build()

			got, err := findAllSubscriptionsForModel(ctx, c, tt.modelNamespace, tt.modelName)
			if err != nil {
				t.Fatalf("findAllSubscriptionsForModel() error = %v", err)
			}
			if len(got) != tt.wantCount {
				t.Errorf("findAllSubscriptionsForModel() returned %d subscriptions, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestFindAllAuthPoliciesForModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		modelNamespace string
		modelName      string
		policies       []*maasv1alpha1.MaaSAuthPolicy
		wantCount      int
	}{
		{
			name:           "no policies",
			modelNamespace: "default",
			modelName:      "model1",
			policies:       nil,
			wantCount:      0,
		},
		{
			name:           "one matching policy",
			modelNamespace: "default",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 1,
		},
		{
			name:           "multiple matching policies",
			modelNamespace: "default",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy2", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
							{Name: "model2", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 2,
		},
		{
			name:           "exclude policies being deleted",
			modelNamespace: "default",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "policy2",
						Namespace:         "auth-ns",
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
						Finalizers:        []string{"test-finalizer"},
					},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 1,
		},
		{
			name:           "no matching namespace",
			modelNamespace: "other-ns",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []maasv1alpha1.MaaSAuthPolicy
			for _, policy := range tt.policies {
				objects = append(objects, *policy)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithLists(&maasv1alpha1.MaaSAuthPolicyList{Items: objects}).
				Build()

			got, err := findAllAuthPoliciesForModel(ctx, c, tt.modelNamespace, tt.modelName)
			if err != nil {
				t.Fatalf("findAllAuthPoliciesForModel() error = %v", err)
			}
			if len(got) != tt.wantCount {
				t.Errorf("findAllAuthPoliciesForModel() returned %d policies, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestFindAnySubscriptionForModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		modelNamespace string
		modelName      string
		subscriptions  []*maasv1alpha1.MaaSSubscription
		wantNil        bool
	}{
		{
			name:           "no subscriptions returns nil",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions:  nil,
			wantNil:        true,
		},
		{
			name:           "returns first subscription",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions: []*maasv1alpha1.MaaSSubscription{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantNil: false,
		},
		{
			name:           "multiple items returns non-nil",
			modelNamespace: "default",
			modelName:      "model1",
			subscriptions: []*maasv1alpha1.MaaSSubscription{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sub2", Namespace: "sub-ns"},
					Spec: maasv1alpha1.MaaSSubscriptionSpec{
						ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []maasv1alpha1.MaaSSubscription
			for _, sub := range tt.subscriptions {
				objects = append(objects, *sub)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithLists(&maasv1alpha1.MaaSSubscriptionList{Items: objects}).
				WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
				Build()

			got := findAnySubscriptionForModel(ctx, c, tt.modelNamespace, tt.modelName)
			if (got == nil) != tt.wantNil {
				t.Errorf("findAnySubscriptionForModel() nil = %v, want nil = %v", got == nil, tt.wantNil)
			}
		})
	}
}

func TestFindAnyAuthPolicyForModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		modelNamespace string
		modelName      string
		policies       []*maasv1alpha1.MaaSAuthPolicy
		wantNil        bool
	}{
		{
			name:           "no policies returns nil",
			modelNamespace: "default",
			modelName:      "model1",
			policies:       nil,
			wantNil:        true,
		},
		{
			name:           "returns first policy",
			modelNamespace: "default",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantNil: false,
		},
		{
			name:           "multiple items returns non-nil",
			modelNamespace: "default",
			modelName:      "model1",
			policies: []*maasv1alpha1.MaaSAuthPolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy1", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "policy2", Namespace: "auth-ns"},
					Spec: maasv1alpha1.MaaSAuthPolicySpec{
						ModelRefs: []maasv1alpha1.ModelRef{
							{Name: "model1", Namespace: "default"},
						},
					},
				},
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []maasv1alpha1.MaaSAuthPolicy
			for _, policy := range tt.policies {
				objects = append(objects, *policy)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithLists(&maasv1alpha1.MaaSAuthPolicyList{Items: objects}).
				Build()

			got := findAnyAuthPolicyForModel(ctx, c, tt.modelNamespace, tt.modelName)
			if (got == nil) != tt.wantNil {
				t.Errorf("findAnyAuthPolicyForModel() nil = %v, want nil = %v", got == nil, tt.wantNil)
			}
		})
	}
}
