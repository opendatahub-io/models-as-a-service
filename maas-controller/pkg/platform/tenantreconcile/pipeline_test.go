package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func payloadProcessingEnvoyFilter(ns, efName, gatewayName string, priority *int64) *unstructured.Unstructured {
	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(GVKEnvoyFilter)
	ef.SetNamespace(ns)
	ef.SetName(efName)
	spec := map[string]any{
		"workloadSelector": map[string]any{
			"labels": map[string]any{
				"gateway.networking.k8s.io/gateway-name": gatewayName,
			},
		},
	}
	if priority != nil {
		spec["priority"] = *priority
	}
	ef.Object["spec"] = spec
	return ef
}

func TestPayloadProcessingEnvoyFilterReady(t *testing.T) {
	const (
		gwNS     = "openshift-ingress"
		gwName   = "partner"
		tenantID = "partner"
	)
	efName := PayloadProcessingEnvoyFilterName(tenantID)
	prioOK := PayloadProcessingEnvoyFilterPriority
	prioLow := int64(0)

	t.Run("missing EnvoyFilter", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, "not found")
		assert.Contains(t, detail, efName)
		assert.Contains(t, detail, "404 NR")
	})

	t.Run("missing priority", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(payloadProcessingEnvoyFilter(gwNS, efName, gwName, nil)).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, "priority=missing")
		assert.Contains(t, detail, "404 NR")
	})

	t.Run("priority too low", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(payloadProcessingEnvoyFilter(gwNS, efName, gwName, &prioLow)).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, "priority=0")
	})

	t.Run("wrong gateway workloadSelector", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(payloadProcessingEnvoyFilter(gwNS, efName, "other-gw", &prioOK)).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, "workloadSelector.labels")
		assert.Contains(t, detail, "other-gw")
	})

	t.Run("missing workloadSelector", func(t *testing.T) {
		ef := &unstructured.Unstructured{}
		ef.SetGroupVersionKind(GVKEnvoyFilter)
		ef.SetNamespace(gwNS)
		ef.SetName(efName)
		ef.Object["spec"] = map[string]any{"priority": prioOK}
		c := fake.NewClientBuilder().WithObjects(ef).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, "has no workloadSelector")
	})

	t.Run("ignores default-tenant EnvoyFilter name for secondary tenants", func(t *testing.T) {
		// Secondary tenants must look up payload-processing-<id>, not payload-processing.
		c := fake.NewClientBuilder().WithObjects(
			payloadProcessingEnvoyFilter(gwNS, PayloadProcessingName, gwName, &prioOK),
		).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, detail, efName)
		assert.Contains(t, detail, "not found")
	})

	t.Run("ready", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(payloadProcessingEnvoyFilter(gwNS, efName, gwName, &prioOK)).Build()
		ready, detail, err := PayloadProcessingEnvoyFilterReady(context.Background(), c, gwNS, gwName, tenantID)
		require.NoError(t, err)
		assert.True(t, ready)
		assert.Empty(t, detail)
	})
}
