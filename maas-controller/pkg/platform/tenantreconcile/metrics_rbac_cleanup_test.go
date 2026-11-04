package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const metricsTestConfigUID = types.UID("config-uid")

func legacyMetricsOwner() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: "default", UID: metricsTestConfigUID,
	}
}

func legacyMetricsRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: "maas-metrics-scraper", UID: "role-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{legacyMetricsOwner()},
	}}
}

func legacyMetricsBinding(name string, owned bool) *rbacv1.ClusterRoleBinding {
	meta := metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), ResourceVersion: "1"}
	if owned {
		meta.OwnerReferences = []metav1.OwnerReference{legacyMetricsOwner()}
	}
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: meta,
		RoleRef:    rbacv1.RoleRef{Kind: "ClusterRole", Name: "maas-metrics-scraper"},
	}
}

func metricsAPIDeployment(tenantID string, status appsv1.DeploymentStatus) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: MaaSAPIDeploymentName(tenantID), Namespace: "maas-infra", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     status,
	}
}

func metricsRBACClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func assertMetricsBindingMissing(t *testing.T, c client.Client, name string) {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Name: name}, &rbacv1.ClusterRoleBinding{})
	assert.True(t, apierrors.IsNotFound(err), "binding %s: %v", name, err)
}

func TestCleanupLegacyMetricsScraperRBACWaitsForEachTenantRollout(t *testing.T) {
	ctx := context.Background()
	defaultBinding := legacyMetricsBinding("maas-metrics-scraper", true)
	teamBinding := legacyMetricsBinding("maas-metrics-scraper-team", true)
	ready := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	// The ordinary readiness check would pass here: updated and available each
	// equal desired, but an old pod still exists and the new pod is not ready.
	rolling := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	teamDeployment := metricsAPIDeployment("team", rolling)
	c := metricsRBACClient(t, legacyMetricsRole(), defaultBinding, teamBinding,
		metricsAPIDeployment("", ready), teamDeployment)

	pending, err := cleanupLegacyMetricsScraperRBAC(ctx, c, c, metricsTestConfigUID, "maas-infra", "", logr.Discard())
	require.NoError(t, err)
	assert.Empty(t, pending)
	assertMetricsBindingMissing(t, c, defaultBinding.Name)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: teamBinding.Name}, &rbacv1.ClusterRoleBinding{}))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "maas-metrics-scraper"}, &rbacv1.ClusterRole{}))

	pending, err = cleanupLegacyMetricsScraperRBAC(ctx, c, c, metricsTestConfigUID, "maas-infra", "team", logr.Discard())
	require.NoError(t, err)
	assert.NotEmpty(t, pending, "the current tenant must requeue until old pods are gone")
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: teamBinding.Name}, &rbacv1.ClusterRoleBinding{}))

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(teamDeployment), teamDeployment))
	teamDeployment.Status = ready
	require.NoError(t, c.Status().Update(ctx, teamDeployment))
	pending, err = cleanupLegacyMetricsScraperRBAC(ctx, c, c, metricsTestConfigUID, "maas-infra", "team", logr.Discard())
	require.NoError(t, err)
	assert.Empty(t, pending)
	assertMetricsBindingMissing(t, c, teamBinding.Name)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Name: "maas-metrics-scraper"}, &rbacv1.ClusterRole{})))
}

func TestCleanupLegacyMetricsScraperRBACWithoutRole(t *testing.T) {
	ctx := context.Background()
	binding := legacyMetricsBinding("maas-metrics-scraper", true)
	ready := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	c := metricsRBACClient(t, binding, metricsAPIDeployment("", ready))

	pending, err := cleanupLegacyMetricsScraperRBAC(ctx, c, c, metricsTestConfigUID, "maas-infra", "", logr.Discard())
	require.NoError(t, err)
	assert.Empty(t, pending)
	assertMetricsBindingMissing(t, c, binding.Name)
}

func TestCleanupLegacyMetricsScraperRBACKeepsForeignBinding(t *testing.T) {
	ctx := context.Background()
	owned := legacyMetricsBinding("maas-metrics-scraper", true)
	foreign := legacyMetricsBinding("external-metrics-binding", false)
	ready := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	c := metricsRBACClient(t, legacyMetricsRole(), owned, foreign, metricsAPIDeployment("", ready))

	pending, err := cleanupLegacyMetricsScraperRBAC(ctx, c, c, metricsTestConfigUID, "maas-infra", "", logr.Discard())
	require.NoError(t, err)
	assert.Empty(t, pending)
	assertMetricsBindingMissing(t, c, owned.Name)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: foreign.Name}, &rbacv1.ClusterRoleBinding{}))
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: "maas-metrics-scraper"}, &rbacv1.ClusterRole{}))
}
