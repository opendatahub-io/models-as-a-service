package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestConvertToFQDNConnectionURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		namespace string
		want      string
	}{
		{
			name:      "simple hostname with port",
			url:       "postgresql://user:pass@postgres:5432/db",
			namespace: "opendatahub",
			want:      "postgresql://user:pass@postgres.opendatahub.svc.cluster.local:5432/db",
		},
		{
			name:      "hostname without port",
			url:       "postgresql://user:pass@postgres/db",
			namespace: "redhat-ods-applications",
			want:      "postgresql://user:pass@postgres.redhat-ods-applications.svc.cluster.local/db",
		},
		{
			name:      "already FQDN - no change",
			url:       "postgresql://user:pass@postgres.opendatahub.svc.cluster.local:5432/db",
			namespace: "opendatahub",
			want:      "postgresql://user:pass@postgres.opendatahub.svc.cluster.local:5432/db",
		},
		{
			name:      "hostname with query params",
			url:       "postgresql://user:pass@postgres:5432/db?sslmode=require",
			namespace: "opendatahub",
			want:      "postgresql://user:pass@postgres.opendatahub.svc.cluster.local:5432/db?sslmode=require",
		},
		{
			name:      "external FQDN hostname",
			url:       "postgresql://user:pass@db.example.com:5432/db",
			namespace: "opendatahub",
			want:      "postgresql://user:pass@db.example.com:5432/db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToFQDNConnectionURL(tt.url, tt.namespace)
			if got != tt.want {
				t.Errorf("convertToFQDNConnectionURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaskConnectionURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "with password",
			url:  "postgresql://user:password@host:5432/db",
			want: "postgresql://user:xxxxx@host:5432/db",
		},
		{
			name: "no password",
			url:  "postgresql://user@host:5432/db",
			want: "postgresql://user@host:5432/db",
		},
		{
			name: "no credentials",
			url:  "postgresql://host:5432/db",
			want: "postgresql://host:5432/db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskConnectionURL(tt.url)
			if got != tt.want {
				t.Errorf("maskConnectionURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMigrateMaaSDBSecretToInfraNamespace(t *testing.T) {
	const (
		secretName      = "maas-db-config" //nolint:gosec // Kubernetes Secret resource name, not a credential
		secretKey       = "DB_CONNECTION_URL"
		controllerNs    = "opendatahub"
		infraNs         = "odh-ai-gateway-infra"
		originalConnURL = "postgresql://maas:password@postgres:5432/maas"
		expectedFQDN    = "postgresql://maas:password@postgres.opendatahub.svc.cluster.local:5432/maas"
	)

	tests := []struct {
		name              string
		setupSecrets      func(*fake.Clientset)
		controllerNs      string
		infraNs           string
		expectError       bool
		expectSecretInDst bool
		expectFQDN        bool
	}{
		{
			name: "upgrade scenario - migrate secret with FQDN",
			setupSecrets: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Secrets(controllerNs).Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: controllerNs},
					Data:       map[string][]byte{secretKey: []byte(originalConnURL)},
				}, metav1.CreateOptions{})
			},
			controllerNs:      controllerNs,
			infraNs:           infraNs,
			expectSecretInDst: true,
			expectFQDN:        true,
		},
		{
			name:              "fresh install - no secret anywhere",
			setupSecrets:      func(cs *fake.Clientset) {},
			controllerNs:      controllerNs,
			infraNs:           infraNs,
			expectSecretInDst: false,
		},
		{
			name: "already migrated - secret exists in infra ns",
			setupSecrets: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Secrets(infraNs).Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: infraNs},
					Data:       map[string][]byte{secretKey: []byte(expectedFQDN)},
				}, metav1.CreateOptions{})
			},
			controllerNs:      controllerNs,
			infraNs:           infraNs,
			expectSecretInDst: true,
		},
		{
			name:              "same namespace - no separation",
			setupSecrets:      func(cs *fake.Clientset) {},
			controllerNs:      controllerNs,
			infraNs:           controllerNs,
			expectSecretInDst: false,
		},
		{
			name:              "empty infra namespace - no separation",
			setupSecrets:      func(cs *fake.Clientset) {},
			controllerNs:      controllerNs,
			infraNs:           "",
			expectSecretInDst: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			tt.setupSecrets(cs)

			err := migrateMaaSDBSecretToInfraNamespace(context.Background(), tt.controllerNs, tt.infraNs, cs)
			if (err != nil) != tt.expectError {
				t.Fatalf("migrateMaaSDBSecretToInfraNamespace() error = %v, expectError %v", err, tt.expectError)
			}

			if tt.expectSecretInDst && tt.infraNs != "" && tt.infraNs != tt.controllerNs {
				secret, err := cs.CoreV1().Secrets(tt.infraNs).Get(context.Background(), secretName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("expected secret in infra namespace, got error: %v", err)
				}
				if tt.expectFQDN {
					got := string(secret.Data[secretKey])
					if got != expectedFQDN {
						t.Errorf("expected FQDN URL %q, got %q", expectedFQDN, got)
					}
				}
			} else if !tt.expectSecretInDst && tt.infraNs != "" && tt.infraNs != tt.controllerNs {
				_, err := cs.CoreV1().Secrets(tt.infraNs).Get(context.Background(), secretName, metav1.GetOptions{})
				if !errors.IsNotFound(err) {
					t.Fatalf("expected no secret in infra namespace, got: %v", err)
				}
			}
		})
	}
}
