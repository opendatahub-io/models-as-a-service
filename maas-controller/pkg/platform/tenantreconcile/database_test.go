package tenantreconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPostgresHostFromConnectionURL(t *testing.T) {
	host, err := postgresHostFromConnectionURL("postgresql://user:pass@postgres:5432/maas")
	require.NoError(t, err)
	assert.Equal(t, "postgres", host)

	host, err = postgresHostFromConnectionURL("postgresql://user:pass@db.example.com:5432/maas?sslmode=require")
	require.NoError(t, err)
	assert.Equal(t, "db.example.com", host)
}

func TestIsInClusterPostgresHost(t *testing.T) {
	assert.True(t, isInClusterPostgresHost("postgres"))
	assert.True(t, isInClusterPostgresHost("postgres.redhat-ai-gateway-infra.svc.cluster.local"))
	assert.True(t, isInClusterPostgresHost("postgres.redhat-ods-applications.svc"))
	assert.False(t, isInClusterPostgresHost("db.example.com"))
	assert.False(t, isInClusterPostgresHost("mydb.abc123.us-east-1.rds.amazonaws.com"))
}

func TestResolveBundledPostgres(t *testing.T) {
	t.Run("in-cluster short hostname", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: MaaSDBSecretName, Namespace: "redhat-ai-gateway-infra"},
			Data:       map[string][]byte{MaaSDBSecretKey: []byte("postgresql://u:p@postgres:5432/maas")},
		}).Build()

		got, err := resolveBundledPostgres(context.Background(), cl, "redhat-ai-gateway-infra")
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("external hostname", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: MaaSDBSecretName, Namespace: "redhat-ai-gateway-infra"},
			Data:       map[string][]byte{MaaSDBSecretKey: []byte("postgresql://u:p@rds.example.com:5432/maas")},
		}).Build()

		got, err := resolveBundledPostgres(context.Background(), cl, "redhat-ai-gateway-infra")
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("missing secret", func(t *testing.T) {
		cl := fake.NewClientBuilder().Build()
		got, err := resolveBundledPostgres(context.Background(), cl, "redhat-ai-gateway-infra")
		require.NoError(t, err)
		assert.False(t, got)
	})
}
