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

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestAITenantValidator_ValidateCreate(t *testing.T) {
	tests := []struct {
		name        string
		validator   *AITenantValidator
		aitenant    *maasv1alpha1.AITenant
		wantErr     bool
		errContains string
	}{
		{
			name: "allow aitenant in configured namespace",
			validator: &AITenantValidator{
				AITenantNamespace: "ai-tenants",
			},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
			},
			wantErr: false,
		},
		{
			name: "reject aitenant outside configured namespace",
			validator: &AITenantValidator{
				AITenantNamespace: "ai-tenants",
			},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "llm",
				},
			},
			wantErr:     true,
			errContains: `configured AITenant infrastructure namespace "ai-tenants"`,
		},
		{
			name: "allow aitenant in custom configured namespace",
			validator: &AITenantValidator{
				AITenantNamespace: "custom-infra",
			},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "custom-infra",
				},
			},
			wantErr: false,
		},
		{
			name:      "reject when validator is nil",
			validator: nil,
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
			},
			wantErr:     true,
			errContains: "webhook validator not configured",
		},
		{
			name:      "reject when configured namespace is empty",
			validator: &AITenantValidator{},
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
			},
			wantErr:     true,
			errContains: "AITenant infrastructure namespace is not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.validator.ValidateCreate(context.Background(), tt.aitenant)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCreate() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.errContains != "" && (err == nil || !contains(err.Error(), tt.errContains)) {
				t.Fatalf("ValidateCreate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestAITenantValidator_ValidateUpdate(t *testing.T) {
	validator := &AITenantValidator{
		AITenantNamespace: "ai-tenants",
	}

	oldAITenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "ai-tenants",
		},
	}
	newAITenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "ai-tenants",
		},
	}

	_, err := validator.ValidateUpdate(context.Background(), oldAITenant, newAITenant)
	if err != nil {
		t.Fatalf("ValidateUpdate() unexpected error: %v", err)
	}
}

func TestAITenantValidator_ValidateDelete(t *testing.T) {
	validator := &AITenantValidator{
		AITenantNamespace: "ai-tenants",
	}

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: "ai-tenants",
		},
	}

	_, err := validator.ValidateDelete(context.Background(), aitenant)
	if err != nil {
		t.Fatalf("ValidateDelete() unexpected error: %v", err)
	}
}
