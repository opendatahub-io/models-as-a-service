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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestAITenantValidator_ValidateCreate_GatewayConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := maasv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	tests := []struct {
		name             string
		existingTenants  []client.Object
		newTenant        *maasv1alpha1.AITenant
		gatewayNamespace string
		wantErr          bool
		errContains      string
	}{
		{
			name:             "allow first tenant with gateway",
			existingTenants:  []client.Object{},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "reject second tenant with same gateway name",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-a",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "shared-gateway",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-b",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "shared-gateway",
					},
				},
			},
			wantErr:     true,
			errContains: "gateway openshift-ingress/shared-gateway is already in use by AITenant ai-tenants/team-a",
		},
		{
			name: "allow second tenant with different gateway",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-a",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "team-a-gateway",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-b",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-b-gateway",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "reject when gateway name defaults to AITenant name and conflicts",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-app",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "my-app",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-app-v2",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "my-app",
					},
				},
			},
			wantErr:     true,
			errContains: "already in use",
		},
		{
			name: "allow when existing tenant uses default name and new uses explicit different name",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-a",
						Namespace: "ai-tenants",
					},
					// No gateway spec, defaults to "team-a"
				},
			},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-b",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-b-gateway",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "reject when both use default names that match",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default-name",
						Namespace: "ai-tenants",
					},
					// No gateway spec, defaults to "default-name"
				},
			},
			gatewayNamespace: "openshift-ingress",
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "new-tenant",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "default-name",
					},
				},
			},
			wantErr:     true,
			errContains: "already in use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingTenants...).
				Build()

			validator := &AITenantValidator{
				Client:            fakeClient,
				AITenantNamespace: "ai-tenants",
				GatewayNamespace:  tt.gatewayNamespace,
			}

			_, err := validator.ValidateCreate(context.Background(), tt.newTenant)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateCreate() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.errContains != "" && (err == nil || !contains(err.Error(), tt.errContains)) {
				t.Fatalf("ValidateCreate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestAITenantValidator_ValidateUpdate_GatewayConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := maasv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}

	tests := []struct {
		name             string
		existingTenants  []client.Object
		oldTenant        *maasv1alpha1.AITenant
		newTenant        *maasv1alpha1.AITenant
		gatewayNamespace string
		wantErr          bool
		errContains      string
	}{
		{
			name:             "allow update when gateway unchanged",
			existingTenants:  []client.Object{},
			gatewayNamespace: "openshift-ingress",
			oldTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
				},
			},
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
					OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
						IssuerURL: "https://example.com",
						ClientID:  "client",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "allow update to unused gateway",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-b",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "team-b-gateway",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			oldTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-old-gateway",
					},
				},
			},
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-new-gateway",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "reject update to gateway already in use",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-b",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "team-b-gateway",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			oldTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
				},
			},
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-b-gateway",
					},
				},
			},
			wantErr:     true,
			errContains: "gateway openshift-ingress/team-b-gateway is already in use by AITenant ai-tenants/team-b",
		},
		{
			name: "allow self-update (same tenant, same gateway)",
			existingTenants: []client.Object{
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "team-a",
						Namespace: "ai-tenants",
					},
					Spec: maasv1alpha1.AITenantSpec{
						Gateway: &maasv1alpha1.AITenantGatewayRef{
							Name: "team-a-gateway",
						},
					},
				},
			},
			gatewayNamespace: "openshift-ingress",
			oldTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
				},
			},
			newTenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "team-a",
					Namespace: "ai-tenants",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "team-a-gateway",
					},
					OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
						IssuerURL: "https://updated.com",
						ClientID:  "client",
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.existingTenants...).
				Build()

			validator := &AITenantValidator{
				Client:            fakeClient,
				AITenantNamespace: "ai-tenants",
				GatewayNamespace:  tt.gatewayNamespace,
			}

			_, err := validator.ValidateUpdate(context.Background(), tt.oldTenant, tt.newTenant)

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateUpdate() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.errContains != "" && (err == nil || !contains(err.Error(), tt.errContains)) {
				t.Fatalf("ValidateUpdate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestAITenantValidator_gatewayRefFor(t *testing.T) {
	tests := []struct {
		name             string
		gatewayNamespace string
		aitenant         *maasv1alpha1.AITenant
		want             maasv1alpha1.TenantGatewayRef
	}{
		{
			name:             "defaults to AITenant name when no gateway spec",
			gatewayNamespace: "openshift-ingress",
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name: "team-a",
				},
			},
			want: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "team-a",
			},
		},
		{
			name:             "uses explicit gateway name when specified",
			gatewayNamespace: "openshift-ingress",
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name: "team-a",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "custom-gateway",
					},
				},
			},
			want: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "custom-gateway",
			},
		},
		{
			name:             "uses AITenant name when gateway spec exists but name is empty",
			gatewayNamespace: "custom-ns",
			aitenant: &maasv1alpha1.AITenant{
				ObjectMeta: metav1.ObjectMeta{
					Name: "team-b",
				},
				Spec: maasv1alpha1.AITenantSpec{
					Gateway: &maasv1alpha1.AITenantGatewayRef{
						Name: "",
					},
				},
			},
			want: maasv1alpha1.TenantGatewayRef{
				Namespace: "custom-ns",
				Name:      "team-b",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &AITenantValidator{
				GatewayNamespace: tt.gatewayNamespace,
			}

			got := validator.gatewayRefFor(tt.aitenant)

			if got.Namespace != tt.want.Namespace || got.Name != tt.want.Name {
				t.Fatalf("gatewayRefFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
