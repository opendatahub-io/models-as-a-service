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

package maas

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func maasAuthPolicyOIDCTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, maasv1alpha1.AddToScheme(scheme))
	return scheme
}

func TestFetchOIDCConfig_NoTenant(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "models-as-a-service")
	assert.Nil(t, config, "should return nil when Tenant doesn't exist")
}

func TestFetchTenantPlatformContext_DiscoveredTenantMissingBridgeFailsClosed(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ai-tenant-team-a",
			Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:                          client,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
		GatewayName:                     "maas-default-gateway",
		GatewayNamespace:                "openshift-ingress",
	}

	platformContext, err := reconciler.fetchTenantPlatformContext(context.Background(), logr.Discard(), "ai-tenant-team-a")

	assert.Nil(t, platformContext)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to use default platform context")
}

func TestFetchOIDCConfig_NoExternalOIDC(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)

	// Create Tenant without externalOIDC
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "models-as-a-service",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant).
		Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "models-as-a-service")
	assert.Nil(t, config, "should return nil when externalOIDC is not configured")
}

func TestFetchOIDCConfig_WithExternalOIDC(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)

	// Create Tenant with externalOIDC
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.TenantSpec{
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://keycloak.example.com/realms/test",
				ClientID:  "test-client",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant).
		Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "models-as-a-service")
	assert.NotNil(t, config, "should return config when externalOIDC is configured")
	assert.Equal(t, "https://keycloak.example.com/realms/test", config.IssuerURL)
	assert.Equal(t, "test-client", config.ClientID)
}

func TestFetchOIDCConfig_WithAITenantOIDC(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)

	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-team-a",
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "team-a",
				tenantreconcile.LabelTenantNamespace:   "ai-tenant-team-a",
			},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      "team-a",
				tenantreconcile.AnnotationAITenantNamespace: tenantreconcile.DefaultAITenantNamespace,
			},
		},
		Spec: maasv1alpha1.TenantSpec{
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://stale.example.com",
				ClientID:  "stale-client",
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "team-a",
			Namespace: tenantreconcile.DefaultAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{
			OIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://keycloak.example.com/realms/team-a",
				ClientID:  "team-a-client",
			},
		},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "openshift-ingress",
				Name:      "team-a-gateway",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant, aitenant).
		Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "ai-tenant-team-a")
	assert.NotNil(t, config, "should return config from AITenant")
	assert.Equal(t, "https://keycloak.example.com/realms/team-a", config.IssuerURL)
	assert.Equal(t, "team-a-client", config.ClientID)
}

func TestFetchOIDCConfig_EmptyIssuerURL(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)

	// Create Tenant with empty issuerUrl
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.TenantSpec{
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "",
				ClientID:  "test-client",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant).
		Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "models-as-a-service")
	assert.Nil(t, config, "should return nil when issuerUrl is empty")
}

func TestFetchOIDCConfig_EmptyClientID(t *testing.T) {
	scheme := maasAuthPolicyOIDCTestScheme(t)

	// Create Tenant with empty clientId
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.TenantSpec{
			ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
				IssuerURL: "https://keycloak.example.com/realms/test",
				ClientID:  "",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tenant).
		Build()

	reconciler := &MaaSAuthPolicyReconciler{
		Client:          client,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	config := reconciler.fetchOIDCConfig(context.Background(), logr.Discard(), "models-as-a-service")
	assert.Nil(t, config, "should return nil when clientId is empty (audience validation required)")
}

func TestCELExpressions_SupportOIDC(t *testing.T) {
	// Test that CEL expressions include OIDC identity fields
	assert.Contains(t, celUserID, "auth.identity.preferred_username",
		"celUserID should check for OIDC preferred_username")
	assert.Contains(t, celUserID, "auth.identity.sub",
		"celUserID should check for OIDC sub claim")
	assert.Contains(t, celUserID, "auth.identity.user.username",
		"celUserID should fall back to K8s user.username")

	assert.Contains(t, celUsername, "auth.identity.preferred_username",
		"celUsername should check for OIDC preferred_username")
	assert.Contains(t, celUsername, "auth.identity.sub",
		"celUsername should check for OIDC sub claim")
	assert.Contains(t, celUsername, "auth.identity.user.username",
		"celUsername should fall back to K8s user.username")

	assert.Contains(t, celGroups, "auth.identity.groups",
		"celGroups should check for OIDC groups claim")
	assert.Contains(t, celGroups, "auth.identity.user.groups",
		"celGroups should fall back to K8s user.groups")
	assert.Contains(t, celTokenGroupsHeaderJSON, "size(auth.identity.groups) > 0",
		"X-MaaS-Group expression should handle empty OIDC groups claim")
	assert.Contains(t, celTokenGroupsHeaderJSON, `'["system:authenticated"]'`,
		"X-MaaS-Group expression should preserve default subscription access for OIDC tokens with no groups")
	assert.Contains(t, celTokenGroupsHeaderJSON, "size(auth.identity.user.groups) > 0",
		"X-MaaS-Group expression should avoid empty JSON group values for K8s tokens")
	assert.Contains(t, celTokenGroupsHeaderJSON, `'["system:authenticated","' + auth.identity.user.groups.join('","') + '"]'`,
		"X-MaaS-Group expression should preserve default subscription access for K8s user.groups tokens")
	assert.Contains(t, celTokenGroupsHeaderJSON, "auth.identity.groups.all",
		"X-MaaS-Group expression should validate OIDC groups before JSON string construction")
	assert.Contains(t, celTokenGroupsHeaderJSON, safeGroupNamePattern,
		"X-MaaS-Group expression should use the safe group-name pattern")
}

func TestCELExpressions_UserIDVsUsername(t *testing.T) {
	// Test that celUserID uses userId (UUID for cache keys)
	assert.Contains(t, celUserID, "auth.metadata.apiKeyValidation.userId",
		"celUserID should use userId for API key cache keys (UUID)")

	// Test that celUsername uses username (service account name for authz)
	assert.Contains(t, celUsername, "auth.metadata.apiKeyValidation.username",
		"celUsername should use username for API key authorization (service account name)")

	// Verify celUserID does NOT use .username (it should use .userId)
	assert.NotContains(t, celUserID, "apiKeyValidation.username",
		"celUserID should NOT use username field")

	// Verify celUsername does NOT use .userId (it should use .username)
	assert.NotContains(t, celUsername, "apiKeyValidation.userId",
		"celUsername should NOT use userId field")
}

func TestCELExpressions_OrderMatters(t *testing.T) {
	// Verify that OIDC checks come before K8s checks
	// This is important because OIDC and K8s identity structures differ

	// For username: should check preferred_username before user.username
	preferredIdx := findSubstring(celUserID, "preferred_username")
	userUsernameIdx := findSubstring(celUserID, "user.username")
	assert.True(t, preferredIdx >= 0, "should check for preferred_username")
	assert.True(t, userUsernameIdx >= 0, "should check for user.username")
	assert.True(t, preferredIdx < userUsernameIdx,
		"should check preferred_username (OIDC) before user.username (K8s)")

	// For groups: should check auth.identity.groups before auth.identity.user.groups
	identityGroupsIdx := findSubstring(celGroups, "auth.identity.groups")
	userGroupsIdx := findSubstring(celGroups, "auth.identity.user.groups")
	assert.True(t, identityGroupsIdx >= 0, "should check for auth.identity.groups")
	assert.True(t, userGroupsIdx >= 0, "should check for auth.identity.user.groups")
	assert.True(t, identityGroupsIdx < userGroupsIdx,
		"should check auth.identity.groups (OIDC) before auth.identity.user.groups (K8s)")
}

// Helper function to find substring index
func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
