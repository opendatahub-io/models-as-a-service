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

package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const testAITenantNS = "ai-tenants"

func TestMaaSModelRefValidator_ValidateCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = maasv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		model       *maasv1alpha1.MaaSModelRef
		aitenant    *maasv1alpha1.AITenant
		wantErr     bool
		errContains string
	}{
		{
			name: "allow model with tenantRef pointing to existing AITenant",
			model: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
					TenantRef: "redteam",
				},
			},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "redteam",
					Namespace: testAITenantNS,
				},
			},
			wantErr: false,
		},
		{
			name: "reject model with tenantRef pointing to non-existent AITenant",
			model: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
					TenantRef: "nonexistent",
				},
			},
			wantErr:     true,
			errContains: "does not exist",
		},
		{
			name: "allow model with empty tenantRef",
			model: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.aitenant != nil {
				objs = append(objs, tt.aitenant)
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

			validator := &MaaSModelRefValidator{
				Client:            client,
				AITenantNamespace: testAITenantNS,
			}

			_, err := validator.ValidateCreate(context.Background(), tt.model)

			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCreate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.errContains != "" && (err == nil || !contains(err.Error(), tt.errContains)) {
				t.Errorf("ValidateCreate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestMaaSModelRefValidator_ValidateUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = maasv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		oldModel    *maasv1alpha1.MaaSModelRef
		newModel    *maasv1alpha1.MaaSModelRef
		aitenant    *maasv1alpha1.AITenant
		wantErr     bool
		errContains string
	}{
		{
			name: "allow update with tenantRef pointing to existing AITenant",
			oldModel: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
				},
			},
			newModel: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
					TenantRef: "redteam",
				},
			},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "redteam",
					Namespace: testAITenantNS,
				},
			},
			wantErr: false,
		},
		{
			name: "reject update with tenantRef pointing to non-existent AITenant",
			oldModel: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
				},
			},
			newModel: &maasv1alpha1.MaaSModelRef{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "model-ns",
				},
				Spec: maasv1alpha1.MaaSModelSpec{
					ModelRef: maasv1alpha1.ModelReference{
						Kind: "LLMInferenceService",
						Name: "test-llmisvc",
					},
					TenantRef: "nonexistent",
				},
			},
			wantErr:     true,
			errContains: "does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.aitenant != nil {
				objs = append(objs, tt.aitenant)
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

			validator := &MaaSModelRefValidator{
				Client:            client,
				AITenantNamespace: testAITenantNS,
			}

			_, err := validator.ValidateUpdate(context.Background(), tt.oldModel, tt.newModel)

			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUpdate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.errContains != "" && (err == nil || !contains(err.Error(), tt.errContains)) {
				t.Errorf("ValidateUpdate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestMaaSModelRefValidator_ValidateDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = maasv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := &MaaSModelRefValidator{
		Client:            client,
		AITenantNamespace: testAITenantNS,
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "model-ns",
		},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: "LLMInferenceService",
				Name: "test-llmisvc",
			},
			TenantRef: "some-tenant",
		},
	}

	// Delete should not validate tenantRef
	_, err := validator.ValidateDelete(context.Background(), model)
	if err != nil {
		t.Errorf("ValidateDelete() unexpected error: %v", err)
	}
}
