package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
)

func TestLoadDatabaseURLs(t *testing.T) {
	tests := []struct {
		name        string
		secret      *corev1.Secret
		wantWrite   string
		wantRead    string
		expectError string
	}{
		{
			name: "writable key only",
			secret: secretWithData(map[string][]byte{
				"DB_CONNECTION_URL": []byte(" postgresql://write.example.com/db "),
			}),
			wantWrite: "postgresql://write.example.com/db",
			wantRead:  "",
		},
		{
			name: "writable and read keys are trimmed",
			secret: secretWithData(map[string][]byte{
				"DB_CONNECTION_URL":      []byte(" postgresql://write.example.com/db "),
				"DB_READ_CONNECTION_URL": []byte(" postgres://read.example.com/db "),
			}),
			wantWrite: "postgresql://write.example.com/db",
			wantRead:  "postgres://read.example.com/db",
		},
		{
			name: "empty read key is optional",
			secret: secretWithData(map[string][]byte{
				"DB_CONNECTION_URL":      []byte("postgresql://write.example.com/db"),
				"DB_READ_CONNECTION_URL": []byte("   "),
			}),
			wantWrite: "postgresql://write.example.com/db",
			wantRead:  "",
		},
		{
			name: "missing writable key",
			secret: secretWithData(map[string][]byte{
				"DB_READ_CONNECTION_URL": []byte("postgresql://read.example.com/db"),
			}),
			expectError: `key "DB_CONNECTION_URL" not found or empty`,
		},
		{
			name: "invalid writable scheme",
			secret: secretWithData(map[string][]byte{
				"DB_CONNECTION_URL": []byte("mysql://write.example.com/db"),
			}),
			expectError: `invalid "DB_CONNECTION_URL"`,
		},
		{
			name: "invalid read scheme",
			secret: secretWithData(map[string][]byte{
				"DB_CONNECTION_URL":      []byte("postgresql://write.example.com/db"),
				"DB_READ_CONNECTION_URL": []byte("mysql://read.example.com/db"),
			}),
			expectError: `invalid "DB_READ_CONNECTION_URL"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Namespace: "maas"}
			clientset := fake.NewClientset(test.secret)

			err := cfg.LoadDatabaseURLs(context.Background(), clientset)

			if test.expectError != "" {
				require.ErrorContains(t, err, test.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantWrite, cfg.DBConnectionURL)
			assert.Equal(t, test.wantRead, cfg.DBReadConnectionURL)
		})
	}
}

func secretWithData(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "maas-db-config", Namespace: "maas"},
		Data:       data,
	}
}
