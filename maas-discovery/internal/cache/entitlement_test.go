package cache

import (
	"context"
	"testing"
	"time"

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

	subscriptions := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"modelRefs": []any{
					map[string]any{"name": "model-a", "namespace": "models-as-a-service"},
				},
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
			"spec": map[string]any{
				"modelRefs": []any{
					map[string]any{"name": "model-a", "namespace": "models-as-a-service"},
				},
				"subjects": map[string]any{"users": []any{"alice@example.com"}},
			},
		}},
	}
	subscriptions := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"modelRefs": []any{
					map[string]any{"name": "model-a", "namespace": "models-as-a-service"},
				},
				"owner": map[string]any{"users": []any{"alice@example.com"}},
			},
			"status": map[string]any{"phase": "Pending"},
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

func TestComputeTenantSubjects_RequiresModelRefIntersection(t *testing.T) {
	tenant := "team-a"
	namespaceToTenant := map[string]string{"ai-tenant-team-a": tenant}

	policies := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"modelRefs": []any{map[string]any{"name": "model-a", "namespace": "models-as-a-service"}},
				"subjects":  map[string]any{"users": []any{"alice@example.com"}},
			},
		}},
	}

	subscriptions := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"namespace": "ai-tenant-team-a"},
			"spec": map[string]any{
				"modelRefs": []any{map[string]any{"name": "model-b", "namespace": "models-as-a-service"}},
				"owner":     map[string]any{"users": []any{"alice@example.com"}},
			},
			"status": map[string]any{"phase": subscriptionPhaseOK},
		}},
	}

	result := computeTenantSubjects(tenant, policies, subscriptions, namespaceToTenant)
	assert.Empty(t, result)
}

func TestSubscriptionEligible_RejectsDeletingSubscription(t *testing.T) {
	obj := map[string]any{
		"metadata": map[string]any{"deletionTimestamp": "2026-01-02T03:04:05Z"},
		"status":   map[string]any{"phase": subscriptionPhaseOK},
	}

	assert.False(t, subscriptionEligible(obj))
}

func TestMarkAllTenantsDirty_IncludesIndexedTenantNames(t *testing.T) {
	ic := &InformerCache{
		tenants: []types.TenantInfo{{Name: "tenant-a"}},
		subjectsByTenant: map[string]map[string]struct{}{
			"tenant-b": {"u:alice": struct{}{}},
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
