package maas

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func TestEnsureGatewayIdentityToken_CreatesSecretWhenMissing(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	token, err := ensureGatewayIdentityToken(ctx, c, "opendatahub", "")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "opendatahub",
		Name:      tenantreconcile.MaaSGatewayIdentitySecretName,
	}, secret))
	assert.Equal(t, []byte(token), secret.Data[tenantreconcile.MaaSGatewayIdentitySecretKey])
	assert.Equal(t, "maas-controller", secret.Labels["app.kubernetes.io/managed-by"])
}

func TestEnsureGatewayIdentityToken_UsesConfiguredTokenOnCreate(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	token, err := ensureGatewayIdentityToken(ctx, c, "opendatahub", "configured-token")
	require.NoError(t, err)
	assert.Equal(t, "configured-token", token)

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "opendatahub",
		Name:      tenantreconcile.MaaSGatewayIdentitySecretName,
	}, secret))
	assert.Equal(t, []byte("configured-token"), secret.Data[tenantreconcile.MaaSGatewayIdentitySecretKey])
}

func TestEnsureGatewayIdentityToken_ReturnsExistingSecret(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.MaaSGatewayIdentitySecretName,
			Namespace: "opendatahub",
		},
		Data: map[string][]byte{
			tenantreconcile.MaaSGatewayIdentitySecretKey: []byte("existing-token"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	token, err := ensureGatewayIdentityToken(ctx, c, "opendatahub", "configured-token")
	require.NoError(t, err)
	assert.Equal(t, "existing-token", token)
}
