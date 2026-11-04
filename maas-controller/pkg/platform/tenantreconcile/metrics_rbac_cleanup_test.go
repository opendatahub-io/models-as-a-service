package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanupLegacyMetricsScraperRBAC(t *testing.T) {
	const configUID = types.UID("config-uid")
	owner := metav1.OwnerReference{
		APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: "default", UID: configUID,
	}
	scheme := runtime.NewScheme()
	require.NoError(t, rbacv1.AddToScheme(scheme))
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: "maas-metrics-scraper", UID: "role-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{owner},
	}}
	defaultBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "maas-metrics-scraper", UID: "binding-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{owner},
	}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "maas-metrics-scraper"}}
	teamBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "maas-metrics-scraper-team", UID: "team-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{owner},
	}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "maas-metrics-scraper"}}
	foreignBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name: "external-metrics-binding", UID: "foreign-uid", ResourceVersion: "1",
	}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "maas-metrics-scraper"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(role, defaultBinding, teamBinding, foreignBinding).Build()

	require.NoError(t, cleanupLegacyMetricsScraperRBAC(context.Background(), c, c, configUID, logr.Discard()))
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKey{Name: role.Name}, &rbacv1.ClusterRole{})))
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKey{Name: defaultBinding.Name}, &rbacv1.ClusterRoleBinding{})))
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKey{Name: teamBinding.Name}, &rbacv1.ClusterRoleBinding{})))
	var kept rbacv1.ClusterRoleBinding
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: foreignBinding.Name}, &kept))
	assert.Equal(t, foreignBinding.UID, kept.UID)
}
