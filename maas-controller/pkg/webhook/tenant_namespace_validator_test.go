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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestTenantNamespaceValidator_ValidateNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = maasv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		namespace      string
		namespaceObj   *corev1.Namespace
		tenantObj      *maasv1alpha1.Tenant
		expectedAllow  bool
		expectedErrMsg string
	}{
		{
			name:      "allow namespace with Tenant CR",
			namespace: "ai-tenant-redteam",
			tenantObj: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "ai-tenant-redteam",
				},
			},
			expectedAllow: true,
		},
		{
			name:      "allow ai-tenant-default with Tenant CR",
			namespace: "ai-tenant-default",
			tenantObj: &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default-tenant",
					Namespace: "ai-tenant-default",
				},
			},
			expectedAllow: true,
		},
		{
			name:           "reject namespace without Tenant CR",
			namespace:      "random-namespace",
			expectedAllow:  false,
			expectedErrMsg: "not enabled for MaaS tenant resources",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with the namespace and Tenant if provided
			var objs []runtime.Object
			if tt.namespaceObj != nil {
				objs = append(objs, tt.namespaceObj)
			}
			if tt.tenantObj != nil {
				objs = append(objs, tt.tenantObj)
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

			validator := &TenantNamespaceValidator{
				Client: client,
			}

			allowed, errMsg := validator.ValidateNamespace(context.Background(), tt.namespace)

			if allowed != tt.expectedAllow {
				t.Errorf("ValidateNamespace() allowed = %v, want %v", allowed, tt.expectedAllow)
			}

			if tt.expectedErrMsg != "" {
				if errMsg == "" {
					t.Errorf("ValidateNamespace() expected error message containing %q, got empty string", tt.expectedErrMsg)
				} else if !contains(errMsg, tt.expectedErrMsg) {
					t.Errorf("ValidateNamespace() error message = %q, want to contain %q", errMsg, tt.expectedErrMsg)
				}
			} else if errMsg != "" {
				t.Errorf("ValidateNamespace() unexpected error message: %q", errMsg)
			}
		})
	}
}

func TestTenantNamespaceValidator_ErrorMessageContent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = maasv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := &TenantNamespaceValidator{
		Client: client,
	}

	_, errMsg := validator.ValidateNamespace(context.Background(), "test-namespace")

	expectedPhrase := "Create a Tenant CR"
	if !contains(errMsg, expectedPhrase) {
		t.Errorf("Error message missing expected phrase %q. Full message: %q", expectedPhrase, errMsg)
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
