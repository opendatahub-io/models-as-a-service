package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func lokiSecret(appNs string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: LokiReadSecretName, Namespace: appNs},
		Data:       data,
	}
}

func TestResolveLokiReadEndpoint(t *testing.T) {
	const appNs = "opendatahub"

	tests := []struct {
		name                 string
		secret               *corev1.Secret
		expectedEndpoint     string
		expectedTokenFromRef bool
		expectWarning        bool
	}{
		{
			name:                 "secret absent disables the proxy",
			secret:               nil,
			expectedEndpoint:     "",
			expectedTokenFromRef: false,
		},
		{
			name: "secret with endpoint and token enables the proxy",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://data-science-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("sa-token-123"),
			}),
			expectedEndpoint:     "https://data-science-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: true,
		},
		{
			name: "missing token key keeps the proxy enabled without upstream auth and warns",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey: []byte("https://loki.example.svc:8080/api/logs/v1/application"),
			}),
			expectedEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: false,
			expectWarning:        true,
		},
		{
			name: "empty token key keeps the proxy enabled without upstream auth and warns",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://loki.example.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("   "),
			}),
			expectedEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: false,
			expectWarning:        true,
		},
		{
			name: "surrounding whitespace is trimmed",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("  https://loki.example.svc:8080/api/logs/v1/application \n"),
				LokiReadTokenSecretKey: []byte("sa-token-123"),
			}),
			expectedEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: true,
		},
		{
			name:             "missing key disables the proxy with a warning",
			secret:           lokiSecret(appNs, map[string][]byte{"OTHER_KEY": []byte("https://loki.example.svc")}),
			expectedEndpoint: "",
			expectWarning:    true,
		},
		{
			name:             "empty key value disables the proxy with a warning",
			secret:           lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("   ")}),
			expectedEndpoint: "",
			expectWarning:    true,
		},
		{
			name:             "non-URL value disables the proxy with a warning",
			secret:           lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("not-a-url")}),
			expectedEndpoint: "",
			expectWarning:    true,
		},
		{
			name:             "unsupported scheme disables the proxy with a warning",
			secret:           lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("ftp://loki.example.svc")}),
			expectedEndpoint: "",
			expectWarning:    true,
		},
		{
			name:             "missing host disables the proxy with a warning",
			secret:           lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("https://")}),
			expectedEndpoint: "",
			expectWarning:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := newSSAFakeClientBuilder(praxisTestScheme(t))
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			c := builder.Build()

			params := PlatformParams{}
			require.NoError(t, resolveLokiReadEndpoint(context.Background(), c, appNs, &params, logr.Discard()))

			require.Equal(t, tt.expectedEndpoint, params.LokiReadEndpoint)
			require.Equal(t, tt.expectedTokenFromRef, params.LokiReadTokenFromSecret)
			if tt.expectWarning {
				require.NotEmpty(t, params.Warnings, "expected a warning to be recorded")
			} else {
				require.Empty(t, params.Warnings)
			}
		})
	}
}

func TestResolveLokiReadEndpoint_APIClientError(t *testing.T) {
	c := failingGetClient{err: errors.New("cluster unavailable")}
	params := PlatformParams{}
	err := resolveLokiReadEndpoint(context.Background(), c, "opendatahub", &params, logr.Discard())
	require.ErrorContains(t, err, "cluster unavailable")
	require.ErrorContains(t, err, LokiReadSecretName)
	require.Empty(t, params.LokiReadEndpoint)
}

type failingGetClient struct {
	client.Client
	err error
}

func (c failingGetClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return c.err
}

func TestPatchMaaSAPIDeploymentLokiReadEndpoint(t *testing.T) {
	makeDeployment := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name":      "maas-api",
					"namespace": "opendatahub",
				},
				"spec": map[string]any{
					"template": map[string]any{
						"spec": map[string]any{
							"containers": []any{
								map[string]any{
									"name": "maas-api",
									"env": []any{
										map[string]any{"name": "GATEWAY_NAMESPACE", "value": "openshift-ingress"},
									},
								},
							},
						},
					},
				},
			},
		}
	}

	envValue := func(dep *unstructured.Unstructured, name string) (string, bool) {
		t.Helper()
		containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		require.True(t, found)
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok || cm["name"] != "maas-api" {
				continue
			}
			envSlice, _ := cm["env"].([]any)
			for _, e := range envSlice {
				if em, ok := e.(map[string]any); ok && em["name"] == name {
					if _, has := em["value"]; !has {
						return "", false
					}
					value, _ := em["value"].(string)
					return value, true
				}
			}
		}
		return "", false
	}

	envSecretRef := func(dep *unstructured.Unstructured, name string) (string, string, bool) {
		t.Helper()
		containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		require.True(t, found)
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok || cm["name"] != "maas-api" {
				continue
			}
			envSlice, _ := cm["env"].([]any)
			for _, e := range envSlice {
				em, ok := e.(map[string]any)
				if !ok || em["name"] != name {
					continue
				}
				ref, _, err := unstructured.NestedString(em, "valueFrom", "secretKeyRef", "name")
				require.NoError(t, err)
				key, _, err := unstructured.NestedString(em, "valueFrom", "secretKeyRef", "key")
				require.NoError(t, err)
				return ref, key, true
			}
		}
		return "", "", false
	}

	t.Run("endpoint set injects LOKI_READ_ENDPOINT", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:     "test-image",
			GatewayNamespace: "openshift-ingress",
			GatewayName:      "test-gateway",
			LokiReadEndpoint: "https://loki.example.svc:8080/api/logs/v1/application",
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		value, ok := envValue(dep, "LOKI_READ_ENDPOINT")
		require.True(t, ok, "LOKI_READ_ENDPOINT env should be present")
		require.Equal(t, "https://loki.example.svc:8080/api/logs/v1/application", value)

		_, _, ok = envSecretRef(dep, "LOKI_READ_TOKEN")
		require.False(t, ok, "LOKI_READ_TOKEN env should be absent without the token flag")
	})

	t.Run("token flag injects LOKI_READ_TOKEN as a secretKeyRef", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			LokiReadEndpoint:        "https://loki.example.svc:8080/api/logs/v1/application",
			LokiReadTokenFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		ref, key, ok := envSecretRef(dep, "LOKI_READ_TOKEN")
		require.True(t, ok, "LOKI_READ_TOKEN env should be present")
		require.Equal(t, LokiReadSecretName, ref)
		require.Equal(t, LokiReadTokenSecretKey, key)

		// The token value must never be copied into the Deployment spec.
		value, ok := envValue(dep, "LOKI_READ_TOKEN")
		require.False(t, ok, "LOKI_READ_TOKEN must use valueFrom, not a plain value")
		require.Empty(t, value)
	})

	t.Run("token flag without endpoint leaves LOKI_READ_TOKEN unset", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			LokiReadTokenFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		_, _, ok := envSecretRef(dep, "LOKI_READ_TOKEN")
		require.False(t, ok, "LOKI_READ_TOKEN env should be absent without an endpoint")
	})

	t.Run("empty endpoint leaves LOKI_READ_ENDPOINT unset", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:     "test-image",
			GatewayNamespace: "openshift-ingress",
			GatewayName:      "test-gateway",
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		_, ok := envValue(dep, "LOKI_READ_ENDPOINT")
		require.False(t, ok, "LOKI_READ_ENDPOINT env should be absent")
	})
}
