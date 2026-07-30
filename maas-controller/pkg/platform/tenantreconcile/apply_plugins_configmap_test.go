package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsPayloadProcessingPluginsConfigMap(t *testing.T) {
	t.Parallel()

	cm := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(GVKConfigMap)
		u.SetName(name)
		return u
	}

	assert.True(t, isPayloadProcessingPluginsConfigMap(cm(PayloadProcessingPluginsConfigMapName)))
	assert.True(t, isPayloadProcessingPluginsConfigMap(cm(PayloadProcessingPluginsConfigMapForTenant("redteam"))))
	assert.False(t, isPayloadProcessingPluginsConfigMap(cm("other-config")))
	assert.False(t, isPayloadProcessingPluginsConfigMap(cm(PayloadProcessingPluginsConfigMapName+"extra")))
	dep := &unstructured.Unstructured{}
	dep.SetKind("Deployment")
	dep.SetName(PayloadProcessingPluginsConfigMapName)
	assert.False(t, isPayloadProcessingPluginsConfigMap(dep))
}

func TestPreparePayloadProcessingPluginsConfigMapApply(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	newRendered := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(GVKConfigMap)
		u.SetNamespace("openshift-ingress")
		u.SetName(PayloadProcessingPluginsConfigMapName)
		return u
	}

	t.Run("stamps managed=false when ConfigMap does not exist", func(t *testing.T) {
		t.Parallel()
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		u := newRendered()
		preparePayloadProcessingPluginsConfigMapApply(context.Background(), c, u)
		assert.Equal(t, "false", u.GetAnnotations()[AnnotationManaged])
	})

	t.Run("stamps managed=false when live has no managed annotation", func(t *testing.T) {
		t.Parallel()
		live := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
		u := newRendered()
		preparePayloadProcessingPluginsConfigMapApply(context.Background(), c, u)
		assert.Equal(t, "false", u.GetAnnotations()[AnnotationManaged])
	})

	t.Run("preserves managed=true when live opted into management", func(t *testing.T) {
		t.Parallel()
		live := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
				Annotations: map[string]string{
					AnnotationManaged: "true",
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
		u := newRendered()
		preparePayloadProcessingPluginsConfigMapApply(context.Background(), c, u)
		assert.Equal(t, "true", u.GetAnnotations()[AnnotationManaged])
	})

	t.Run("ignores non-plugins ConfigMaps", func(t *testing.T) {
		t.Parallel()
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(GVKConfigMap)
		u.SetName("unrelated")
		preparePayloadProcessingPluginsConfigMapApply(context.Background(), c, u)
		assert.Nil(t, u.GetAnnotations())
	})
}
