package tenantreconcile

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Tenant-neutral operands carry no tracking labels, so a second tenant's apply is a
// no-op only if every tenant renders them identically.
func TestTenantNeutralOperandsRenderIdenticallyForEveryTenant(t *testing.T) {
	neutral := func(resources []unstructured.Unstructured) map[string]unstructured.Unstructured {
		out := map[string]unstructured.Unstructured{}
		for _, r := range resources {
			if isTenantNeutralOperand(&r) {
				out[r.GetKind()+"/"+r.GetNamespace()+"/"+r.GetName()] = r
			}
		}
		return out
	}

	defaultTenant := neutral(postRenderTenant(t, "odh", ""))
	require.ElementsMatch(t, []string{"Service/maas-infra/maas-api-metrics", "ServiceMonitor/maas-infra/maas-api-metrics"},
		slices.Collect(maps.Keys(defaultTenant)))
	assert.Equal(t, defaultTenant, neutral(postRenderTenant(t, "odh", "redteam")))
}
