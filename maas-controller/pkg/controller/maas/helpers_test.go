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
