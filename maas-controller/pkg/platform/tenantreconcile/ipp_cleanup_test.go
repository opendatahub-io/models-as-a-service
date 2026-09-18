package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestIsMaaSOwnedIPPResource(t *testing.T) {
	const configUID = types.UID("cfg-uid")

	legacyWithOwner := &unstructured.Unstructured{}
	legacyWithOwner.SetGroupVersionKind(GVKDeployment)
	setConfigControllerOwnerRef(legacyWithOwner, configUID)

	praxisOwned := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"managedFields": []any{
				map[string]any{"manager": aiGatewayControllerFieldOwner},
			},
		},
	}}
	praxisOwned.SetGroupVersionKind(GVKDeployment)

	legacyWithManager := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"managedFields": []any{
				map[string]any{"manager": ssaFieldOwner},
			},
		},
	}}
	legacyWithManager.SetGroupVersionKind(GVKDeployment)

	unmanaged := &unstructured.Unstructured{}
	unmanaged.SetGroupVersionKind(GVKDeployment)
	unmanaged.SetAnnotations(map[string]string{AnnotationManaged: "false"})

	legacyWithTenantLabel := &unstructured.Unstructured{}
	legacyWithTenantLabel.SetGroupVersionKind(GVKDeployment)
	legacyWithTenantLabel.SetLabels(map[string]string{LabelTenantName: "team-a"})

	assert.True(t, isMaaSOwnedIPPResource(legacyWithOwner, configUID))
	assert.False(t, isMaaSOwnedIPPResource(praxisOwned, configUID))
	assert.True(t, isMaaSOwnedIPPResource(legacyWithManager, configUID))
	assert.True(t, isMaaSOwnedIPPResource(unmanaged, configUID), "unmanaged resources must still be deleted on backend switch-off")
	assert.True(t, isMaaSOwnedIPPResource(legacyWithTenantLabel, configUID))

	// Praxis ownership wins over managed=false so we never delete the peer's bundle.
	unmanagedPraxis := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{AnnotationManaged: "false"},
			"managedFields": []any{
				map[string]any{"manager": aiGatewayControllerFieldOwner},
			},
		},
	}}
	unmanagedPraxis.SetGroupVersionKind(GVKDeployment)
	assert.False(t, isMaaSOwnedIPPResource(unmanagedPraxis, configUID))
}

func TestIPPMigrationCleanupMarker(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationIPPMigrationCleanupComplete: "true"},
		},
	}
	assert.True(t, isIPPMigrationCleanupComplete(tenant))
	tenant.SetAnnotations(nil)
	assert.False(t, isIPPMigrationCleanupComplete(tenant))
}
