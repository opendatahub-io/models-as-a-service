package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

func TestComputeTenantSubjects_RequiresSubscriptionAndPolicyMatch(t *testing.T) {
	tenant := "team-a"

	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"subjects": map[string]any{
					"users":  []any{"alice@example.com"},
					"groups": []any{map[string]any{"name": "team-a-users"}},
				},
			},
		}},
	}

	subscriptions := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"owner": map[string]any{
					"users":  []any{"alice@example.com", "bob@example.com"},
					"groups": []any{map[string]any{"name": "team-a-users"}, map[string]any{"name": "no-policy-group"}},
				},
			},
			"status": map[string]any{"phase": subscriptionPhaseOK},
		}},
	}
	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	result := computeTenantSubjects(tenant, policies, subscriptions, namespaceToTenant)

	assert.Contains(t, result, "u:alice@example.com")
	assert.Contains(t, result, "g:team-a-users")
	assert.NotContains(t, result, "u:bob@example.com")
	assert.NotContains(t, result, "g:no-policy-group")
}

func TestComputeTenantSubjects_RejectsIneligibleSubscriptions(t *testing.T) {
	tenant := "team-a"
	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec":     map[string]any{"subjects": map[string]any{"users": []any{"alice@example.com"}}},
		}},
	}
	subscriptions := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec":     map[string]any{"owner": map[string]any{"users": []any{"alice@example.com"}}},
			"status":   map[string]any{"phase": "Pending"},
		}},
	}
	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	result := computeTenantSubjects(tenant, policies, subscriptions, namespaceToTenant)
	assert.Empty(t, result)
}

func TestBuildNamespaceTenantMap(t *testing.T) {
	tenants := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{
				"name": "models-as-a-service",
			},
			"status": map[string]any{"tenantNamespace": "models-as-a-service"},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{
				"name": "team-a",
			},
			"status": map[string]any{"tenantNamespace": "ai-tenant-team-a"},
		}},
	}

	m := buildNamespaceTenantMap(tenants)
	assert.Equal(t, "models-as-a-service", m["models-as-a-service"])
	assert.Equal(t, "team-a", m["ai-tenant-team-a"])
}

func TestListForSubjects(t *testing.T) {
	ic := &InformerCache{
		tenants: []types.TenantInfo{
			{Name: "alpha"},
			{Name: "beta"},
		},
		tenantsBySubject: map[string]map[string]struct{}{
			"u:alice@example.com": {
				"alpha": {},
			},
			"g:team-b": {
				"beta": {},
			},
		},
	}

	visible := ic.ListForSubjects("alice@example.com", []string{"team-b"})
	assert.Len(t, visible, 2)
	assert.Equal(t, "alpha", visible[0].Name)
	assert.Equal(t, "beta", visible[1].Name)

	none := ic.ListForSubjects("charlie@example.com", []string{"unknown"})
	assert.Empty(t, none)
}
