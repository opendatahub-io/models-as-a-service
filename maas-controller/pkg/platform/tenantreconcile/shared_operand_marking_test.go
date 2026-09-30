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

// sharedMarkingTenant builds the tenant config and PlatformParams renderTenantOperands
// renders with, matching how BuildPlatformParams derives TenantTrackingName/Namespace.
func sharedMarkingTenant(t *testing.T, overlayDir, appNamespace, gatewayName, tenantID string, skipIPP bool) []unstructured.Unstructured {
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

	resources, err := PostRender(context.Background(), logr.Discard(), tenant, rendered, params)
	require.NoError(t, err)
	return resources
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

// TestSharedNameCollisionGap documents, without fixing, the gap section 1 of the design
// flags: an AITenant whose name matches the suffix a per-tenant name helper would
// produce for the same kind and namespace as a shared object makes that render collide.
// The fix (reserving the name via a CEL rule) is deliberately out of scope here; this
// pins today's one known collision so a new one introduced by a future rename does not
// slip in silently.
func TestSharedNameCollisionGap(t *testing.T) {
	overlayDir := sharedMarkingOverlayDir(t, "odh")
	shared := sharedMarkingTenant(t, overlayDir, "opendatahub", "maas-default-gateway", "", false)

	// Per-tenant name helpers, keyed by the GVK they render, mirroring what
	// tenantRenderedNames used to enumerate before the shared marker replaced it.
	perTenantNameHelpers := map[schema.GroupVersionKind][]func(tenantID string) string{
		GVKService: {MaaSAPIServiceName, PayloadProcessingServiceName, PayloadPreProcessingServiceName},
	}

	var collisions []string
	for i := range shared {
		obj := shared[i]
		if obj.GetLabels()[LabelSharedOperand] != "true" {
			continue
		}
		for _, helper := range perTenantNameHelpers[obj.GroupVersionKind()] {
			if got := helper("metrics"); got == obj.GetName() {
				collisions = append(collisions, obj.GetKind()+"/"+obj.GetNamespace()+"/"+obj.GetName())
			}
		}
	}

	require.Equal(t, []string{"Service/opendatahub/maas-api-metrics"}, collisions,
		"an AITenant named \"metrics\" collides with the shared maas-api-metrics Service; "+
			"reserving the name is tracked as a follow-up, not fixed here")
}
