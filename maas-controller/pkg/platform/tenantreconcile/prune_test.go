package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPruneLegacyCleanupResources(t *testing.T) {
	t.Run("deletes legacy CronJob and NetworkPolicy", func(t *testing.T) {
		appNs := "tenant-ns"
		cronJob := newLegacyResource(GVKCronJob, LegacyMaaSAPIKeyCleanupCronJobName, appNs, nil)
		networkPolicy := newLegacyResource(GVKNetworkPolicy, LegacyMaaSAPICleanupNetworkPolicyName, appNs, nil)

		c := fake.NewClientBuilder().WithObjects(cronJob, networkPolicy).Build()
		require.NoError(t, PruneLegacyCleanupResources(context.Background(), logr.Discard(), c, appNs))

		assertNotFound(t, c, GVKCronJob, LegacyMaaSAPIKeyCleanupCronJobName, appNs)
		assertNotFound(t, c, GVKNetworkPolicy, LegacyMaaSAPICleanupNetworkPolicyName, appNs)
	})

	t.Run("no-op when legacy resources are absent", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		require.NoError(t, PruneLegacyCleanupResources(context.Background(), logr.Discard(), c, "tenant-ns"))
	})

	t.Run("skips resources with opendatahub.io/managed=false", func(t *testing.T) {
		appNs := "tenant-ns"
		cronJob := newLegacyResource(GVKCronJob, LegacyMaaSAPIKeyCleanupCronJobName, appNs, map[string]string{
			AnnotationManaged: "false",
		})

		c := fake.NewClientBuilder().WithObjects(cronJob).Build()
		require.NoError(t, PruneLegacyCleanupResources(context.Background(), logr.Discard(), c, appNs))

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(GVKCronJob)
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: LegacyMaaSAPIKeyCleanupCronJobName, Namespace: appNs}, obj))
	})
}

func newLegacyResource(gvk schema.GroupVersionKind, name, namespace string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	return obj
}

func assertNotFound(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name, namespace string) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: namespace}, obj)
	require.True(t, apierrors.IsNotFound(err))
}
