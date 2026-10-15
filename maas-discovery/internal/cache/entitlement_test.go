package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

func modelSet(models ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(models))
	for _, model := range models {
		out[model] = struct{}{}
	}
	return out
}

func TestComputeTenantEntitlement_IndexesPolicySubjects(t *testing.T) {
	tenant := "team-a"

	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"modelRefs": []any{
					map[string]any{"name": "model-a", "namespace": "models-as-a-service"},
				},
				"subjects": map[string]any{
					"users":  []any{"alice@example.com"},
					"groups": []any{map[string]any{"name": "team-a-users"}},
				},
			},
		}},
	}

	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	result := computeTenantEntitlement(tenant, policies, namespaceToTenant)

	assert.Contains(t, result.policyModelsBySubject, "u:alice@example.com")
	assert.Contains(t, result.policyModelsBySubject, "g:team-a-users")
	assert.Equal(t, modelSet("models-as-a-service/model-a"), result.policyModelsBySubject["u:alice@example.com"])
}

func TestComputeTenantEntitlement_IgnoresPoliciesWithoutModelRefs(t *testing.T) {
	tenant := "team-a"
	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"subjects": map[string]any{"users": []any{"alice@example.com"}},
			},
		}},
	}
	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	result := computeTenantEntitlement(tenant, policies, namespaceToTenant)
	assert.Empty(t, result.policyModelsBySubject)
	assert.False(t, callerHasTenantAccess(result, []string{"u:alice@example.com"}))
}

func TestComputeTenantEntitlement_IgnoresDeletingPolicies(t *testing.T) {
	tenant := "team-a"
	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{
				"namespace":         "ai-tenant-team-a",
				"deletionTimestamp": "2026-10-12T12:00:00Z",
			},
			"spec": map[string]any{
				"modelRefs": []any{map[string]any{"name": "model-a", "namespace": "models-as-a-service"}},
				"subjects":  map[string]any{"users": []any{"alice@example.com"}},
			},
		}},
	}

	result := computeTenantEntitlement(tenant, policies, namespaceToTenant)
	assert.Empty(t, result.policyModelsBySubject)
	assert.False(t, callerHasTenantAccess(result, []string{"u:alice@example.com"}))
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
		entitlementsByTenant: map[string]tenantEntitlement{
			"alpha": {
				policyModelsBySubject: map[string]map[string]struct{}{
					"u:alice@example.com": modelSet("models-as-a-service/model-a"),
				},
			},
			"beta": {
				policyModelsBySubject: map[string]map[string]struct{}{
					"g:team-b": modelSet("models-as-a-service/model-b"),
				},
			},
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

	aliceOnly := ic.ListForSubjects("alice@example.com", nil)
	assert.Len(t, aliceOnly, 1)
	assert.Equal(t, "alpha", aliceOnly[0].Name)

	none := ic.ListForSubjects("charlie@example.com", []string{"unknown"})
	assert.Empty(t, none)
}

func TestCallerHasTenantAccess_MatchesAnyPolicySubject(t *testing.T) {
	entitlement := tenantEntitlement{
		policyModelsBySubject: map[string]map[string]struct{}{
			"u:alice@example.com": modelSet("models-as-a-service/model-a"),
		},
	}

	assert.True(t, callerHasTenantAccess(entitlement, []string{"u:alice@example.com"}))
}

func TestCallerHasTenantAccess_RejectsWhenNoPolicySubjectMatch(t *testing.T) {
	entitlement := tenantEntitlement{
		policyModelsBySubject: map[string]map[string]struct{}{
			"g:system:authenticated": modelSet("models-as-a-service/model-a"),
		},
	}

	assert.False(t, callerHasTenantAccess(entitlement, []string{"g:team-a"}))
}

func TestMarkAllTenantsDirty_IncludesIndexedTenantNames(t *testing.T) {
	ic := &InformerCache{
		tenants: []types.TenantInfo{{Name: "tenant-a"}},
		entitlementsByTenant: map[string]tenantEntitlement{
			"tenant-b": {
				policyModelsBySubject: map[string]map[string]struct{}{"u:alice": modelSet("models-as-a-service/model-a")},
			},
		},
		dirtyTenants: make(map[string]struct{}),
	}

	ic.markAllTenantsDirty()

	_, hasTenantA := ic.dirtyTenants["tenant-a"]
	_, hasTenantB := ic.dirtyTenants["tenant-b"]
	assert.True(t, hasTenantA)
	assert.True(t, hasTenantB)
}

func TestDrainLoop_MaxWaitPreventsStarvation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rebuildCh := make(chan struct{}, 1)
	done := make(chan struct{})
	debounceTimer := time.NewTimer(rebuildDebounce)
	start := time.Now()

	go func() {
		drainLoop(ctx, rebuildCh, debounceTimer, 120*time.Millisecond)
		close(done)
	}()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			elapsed := time.Since(start)
			assert.Less(t, elapsed, 300*time.Millisecond)
			assert.Greater(t, elapsed, 90*time.Millisecond)
			return
		case <-ticker.C:
			select {
			case rebuildCh <- struct{}{}:
			default:
			}
		case <-time.After(2 * time.Second):
			t.Fatal("drainLoop did not return")
		}
	}
}
