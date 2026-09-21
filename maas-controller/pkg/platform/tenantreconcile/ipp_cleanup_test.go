package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
