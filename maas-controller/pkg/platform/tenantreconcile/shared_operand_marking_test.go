package tenantreconcile

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// sharedMarkingOverlayDir resolves a maas-api overlay directory relative to this file,
// the same way renderOverlayResources (params_test.go) does for "odh".
func sharedMarkingOverlayDir(t *testing.T, overlay string) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "..", "maas-api", "deploy", "overlays", overlay))
}

// sharedMarkingTenant renders tenantID (the default tenant when empty) through
// BuildPlatformParams and PostRender, as its reconcile does.
func sharedMarkingTenant(t *testing.T, overlayDir, appNamespace, gatewayName, tenantID string, skipIPP bool) []unstructured.Unstructured {
	t.Helper()

	resources, err := sharedMarkingRender(t, overlayDir, appNamespace, gatewayName, tenantID, skipIPP)
	require.NoError(t, err)
	return resources
}

func sharedMarkingRender(t *testing.T, overlayDir, appNamespace, gatewayName, tenantID string, skipIPP bool) ([]unstructured.Unstructured, error) {
	t.Helper()

	tenant := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service",
	}}
	if tenantID != "" {
		tenant.Namespace = "ai-tenant-" + tenantID
		tenant.Labels = map[string]string{
			LabelManagedByAITenant: "true",
			LabelTenantName:        tenantID,
		}
	}

	params, err := BuildPlatformParams(tenant, PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: gatewayName},
		SkipIPP:    skipIPP,
	}, appNamespace, appNamespace, "https://kubernetes.default.svc", appNamespace, logr.Discard())
	require.NoError(t, err)

	rendered, err := RenderKustomize(overlayDir, appNamespace)
	require.NoError(t, err)

	return PostRender(context.Background(), logr.Discard(), tenant, rendered, params)
}

type sharedMarkingKey struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

func sharedMarkingKeyOf(u *unstructured.Unstructured) sharedMarkingKey {
	return sharedMarkingKey{gvk: u.GroupVersionKind(), namespace: u.GetNamespace(), name: u.GetName()}
}

// TestSharedOperandMarkingInvariant pins the render-time invariant section 4 of the
// design proposes: an object is per-tenant if and only if a rename site derived its name
// from the tenant, in which case it carries tracking labels; everything else is marked
// shared. It is the regression net for any future per-tenant content sneaking into a
// shared object, and for any rename site that forgets to stamp tracking labels.
func TestSharedOperandMarkingInvariant(t *testing.T) {
	for _, overlay := range []string{"odh", "xks"} {
		t.Run(overlay, func(t *testing.T) {
			overlayDir := sharedMarkingOverlayDir(t, overlay)

			def := sharedMarkingTenant(t, overlayDir, "opendatahub", "maas-default-gateway", "", false)
			red := sharedMarkingTenant(t, overlayDir, "opendatahub", "red", "red", false)
			praxisRed := sharedMarkingTenant(t, overlayDir, "opendatahub", "red", "red", true)

			byKey := map[string]map[sharedMarkingKey]unstructured.Unstructured{}
			for name, set := range map[string][]unstructured.Unstructured{"default": def, "red": red, "praxis-red": praxisRed} {
				keys := map[sharedMarkingKey]unstructured.Unstructured{}
				for i := range set {
					obj := set[i]
					l := obj.GetLabels()
					tracked := l[LabelTenantNamespace] != ""
					shared := l[LabelSharedOperand] == "true"
					require.NotEqualf(t, tracked, shared,
						"%s: %s %s/%s must carry exactly one of tracking labels or the shared marker, got %v",
						name, obj.GetKind(), obj.GetNamespace(), obj.GetName(), l)
					keys[sharedMarkingKeyOf(&obj)] = obj
				}
				byKey[name] = keys
			}

			// The marked set equals the intersection of the tenants' kind/namespace/name
			// sets: every object common to default and red (by identity, not content) is
			// exactly the shared set, on both renders.
			intersection := map[sharedMarkingKey]bool{}
			for k := range byKey["default"] {
				if _, ok := byKey["red"][k]; ok {
					intersection[k] = true
				}
			}
			for name, keys := range byKey {
				if name == "praxis-red" {
					continue // praxis skips IPP resources, so it is not part of the odh/red intersection.
				}
				shared := map[sharedMarkingKey]bool{}
				for k, obj := range keys {
					if obj.GetLabels()[LabelSharedOperand] == "true" {
						shared[k] = true
					}
				}
				require.Equalf(t, intersection, shared, "%s: marked set must equal the default/red kind+namespace+name intersection", name)
			}

			// Marked objects are deep-equal across tenants: this is the regression net for
			// any future per-tenant content in a shared object. Both sides are marked
			// shared, so neither carries tenant tracking labels to strip before comparing.
			for k := range intersection {
				defObj, redObj := byKey["default"][k], byKey["red"][k]
				require.Equal(t, defObj.Object, redObj.Object, "%v must render identically for every tenant", k)
			}
		})
	}
}

// TestPostRenderRejectsDuplicateObjects covers a tenant whose name makes a rename site
// produce a name the render already uses: AITenant "metrics" renames its maas-api Service
// onto the shared maas-api-metrics Service. Applying both would flip that Service on every
// pass and wake the tenants that repair it, a hot loop. Only renaming the tenant helps.
func TestPostRenderRejectsDuplicateObjects(t *testing.T) {
	overlayDir := sharedMarkingOverlayDir(t, "odh")

	t.Run("a tenant whose per-tenant name collides with a shared object", func(t *testing.T) {
		_, err := sharedMarkingRender(t, overlayDir, "opendatahub", "metrics", "metrics", false)
		require.Error(t, err)
		require.ErrorIs(t, err, reconcile.TerminalError(nil))
		require.ErrorContains(t, err, "Service opendatahub/maas-api-metrics")
	})

	t.Run("an ordinary tenant", func(t *testing.T) {
		_, err := sharedMarkingRender(t, overlayDir, "opendatahub", "red", "red", false)
		require.NoError(t, err)
	})
}
