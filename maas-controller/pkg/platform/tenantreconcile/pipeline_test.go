package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSyncMaaSParametersConfigMap_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	params := PlatformParams{APIKeyMaxExpirationDays: "365"}

	err := syncMaaSParametersConfigMap(context.Background(), c, "test-ns", params, logr.Discard())

	assert.NoError(t, err)
}

func TestSyncMaaSParametersConfigMap_AlreadyCorrect(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasParametersConfigMapName,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"api-key-max-expiration-days": "365",
		},
	}
	c := fake.NewClientBuilder().WithObjects(cm).Build()
	params := PlatformParams{APIKeyMaxExpirationDays: "365"}

	err := syncMaaSParametersConfigMap(context.Background(), c, "test-ns", params, logr.Discard())

	assert.NoError(t, err)

	var updated corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: maasParametersConfigMapName, Namespace: "test-ns"}, &updated))
	assert.Equal(t, "365", updated.Data["api-key-max-expiration-days"])
}

func TestSyncMaaSParametersConfigMap_UpdatesValue(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasParametersConfigMapName,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			"api-key-max-expiration-days": "90",
		},
	}
	c := fake.NewClientBuilder().WithObjects(cm).Build()
	params := PlatformParams{APIKeyMaxExpirationDays: "365"}

	err := syncMaaSParametersConfigMap(context.Background(), c, "test-ns", params, logr.Discard())

	assert.NoError(t, err)

	var updated corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: maasParametersConfigMapName, Namespace: "test-ns"}, &updated))
	assert.Equal(t, "365", updated.Data["api-key-max-expiration-days"])
}
