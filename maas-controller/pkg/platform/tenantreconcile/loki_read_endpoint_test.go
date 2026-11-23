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
	const defaultEndpoint = "https://data-science-usage-logs-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"
	const legacyEndpoint = "https://usage-gateway-http.opendatahub.svc:8080/api/logs/v1/application"

	tests := []struct {
		name                 string
		monitoringNamespace  string
		usageLogging         bool
		secret               *corev1.Secret
		expectedEndpoint     string
		expectedTokenFromRef bool
		expectedCAFromRef    bool
		expectWarning        bool
		expectLokiEgressRule bool
	}{
		{
			name:                 "secret absent uses the default LokiStack endpoint",
			monitoringNamespace:  appNs,
			secret:               nil,
			expectedEndpoint:     defaultEndpoint,
			expectLokiEgressRule: true,
		},
		{
			name:                 "usageLogging uses the legacy LokiStack endpoint",
			monitoringNamespace:  appNs,
			usageLogging:         true,
			secret:               nil,
			expectedEndpoint:     legacyEndpoint,
			expectLokiEgressRule: true,
		},
		{
			name:                "secret absent without a monitoring namespace disables the proxy",
			monitoringNamespace: "",
			secret:              nil,
			expectedEndpoint:    "",
		},
		{
			name: "secret with endpoint and token overrides the default",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://data-science-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("sa-token-123"),
			}),
			expectedEndpoint:     "https://data-science-lokistack-gateway-http.opendatahub.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: true,
		},
		{
			name: "CA key injects the CA secretKeyRef without a warning",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:   []byte("https://loki.example.svc:8080/api/logs/v1/application"),
				LokiReadCASecretKey: []byte("-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n"),
			}),
			expectedEndpoint:  "https://loki.example.svc:8080/api/logs/v1/application",
			expectedCAFromRef: true,
		},
		{
			name: "missing CA key falls back to the baked service-ca file without a warning",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://loki.example.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("sa-token-123"),
			}),
			expectedEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: true,
		},
		{
			name: "empty CA key falls back to the baked service-ca file without a warning",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://loki.example.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("sa-token-123"),
				LokiReadCASecretKey:    []byte("   "),
			}),
			expectedEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			expectedTokenFromRef: true,
		},
		{
			name: "missing token key authenticates with the projected ServiceAccount token without a warning",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey: []byte("https://loki.example.svc:8080/api/logs/v1/application"),
			}),
			expectedEndpoint: "https://loki.example.svc:8080/api/logs/v1/application",
		},
		{
			name: "empty token key authenticates with the projected ServiceAccount token without a warning",
			secret: lokiSecret(appNs, map[string][]byte{
				LokiReadSecretKey:      []byte("https://loki.example.svc:8080/api/logs/v1/application"),
				LokiReadTokenSecretKey: []byte("   "),
			}),
			expectedEndpoint: "https://loki.example.svc:8080/api/logs/v1/application",
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
			name:                 "missing key keeps the default endpoint with a warning",
			monitoringNamespace:  appNs,
			secret:               lokiSecret(appNs, map[string][]byte{"OTHER_KEY": []byte("https://loki.example.svc")}),
			expectedEndpoint:     defaultEndpoint,
			expectWarning:        true,
			expectLokiEgressRule: true,
		},
		{
			name:                 "empty key value keeps the default endpoint with a warning",
			monitoringNamespace:  appNs,
			secret:               lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("   ")}),
			expectedEndpoint:     defaultEndpoint,
			expectWarning:        true,
			expectLokiEgressRule: true,
		},
		{
			name:                 "non-URL value keeps the default endpoint with a warning",
			monitoringNamespace:  appNs,
			secret:               lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("not-a-url")}),
			expectedEndpoint:     defaultEndpoint,
			expectWarning:        true,
			expectLokiEgressRule: true,
		},
		{
			name:                 "unsupported scheme keeps the default endpoint with a warning",
			monitoringNamespace:  appNs,
			secret:               lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("ftp://loki.example.svc")}),
			expectedEndpoint:     defaultEndpoint,
			expectWarning:        true,
			expectLokiEgressRule: true,
		},
		{
			name:                 "missing host keeps the default endpoint with a warning",
			monitoringNamespace:  appNs,
			secret:               lokiSecret(appNs, map[string][]byte{LokiReadSecretKey: []byte("https://")}),
			expectedEndpoint:     defaultEndpoint,
			expectWarning:        true,
			expectLokiEgressRule: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := newSSAFakeClientBuilder(praxisTestScheme(t))
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			c := builder.Build()

			params := PlatformParams{MonitoringNamespace: tt.monitoringNamespace}
			require.NoError(t, resolveLokiReadEndpoint(context.Background(), c, appNs, &params, tt.usageLogging, logr.Discard()))

			require.Equal(t, tt.expectedEndpoint, params.LokiReadEndpoint)
			require.Equal(t, tt.expectedTokenFromRef, params.LokiReadTokenFromSecret)
			require.Equal(t, tt.expectedCAFromRef, params.LokiReadCAFromSecret)
			require.Equal(t, tt.expectLokiEgressRule, len(params.MaaSAPILokiEgressRules) == 1)
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
	err := resolveLokiReadEndpoint(context.Background(), c, "opendatahub", &params, false, logr.Discard())
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

	secretVolume := func(dep *unstructured.Unstructured, volumeName string) (string, string, bool) {
		t.Helper()
		volumes, _, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes")
		require.NoError(t, err)
		for _, v := range volumes {
			vm, ok := v.(map[string]any)
			if !ok || vm["name"] != volumeName {
				continue
			}
			secretName, _, _ := unstructured.NestedString(vm, "secret", "secretName")
			items, _, _ := unstructured.NestedSlice(vm, "secret", "items")
			if len(items) == 1 {
				if im, ok := items[0].(map[string]any); ok {
					key, _ := im["key"].(string)
					path, _ := im["path"].(string)
					return secretName, key + ":" + path, true
				}
			}
			return secretName, "", true
		}
		return "", "", false
	}

	hasVolumeMount := func(dep *unstructured.Unstructured, volumeName string) bool {
		t.Helper()
		containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		require.NoError(t, err)
		require.True(t, found)
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok || cm["name"] != "maas-api" {
				continue
			}
			mounts, _ := cm["volumeMounts"].([]any)
			for _, m := range mounts {
				if mm, ok := m.(map[string]any); ok && mm["name"] == volumeName {
					return true
				}
			}
		}
		return false
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

	t.Run("token flag mounts the Secret token and repoints LOKI_READ_TOKEN_FILE", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			LokiReadEndpoint:        "https://loki.example.svc:8080/api/logs/v1/application",
			LokiReadTokenFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		secretName, item, ok := secretVolume(dep, "loki-read-secret-token")
		require.True(t, ok, "loki-read-secret-token volume should be present")
		require.Equal(t, LokiReadSecretName, secretName)
		require.Equal(t, LokiReadTokenSecretKey+":token", item)
		require.True(t, hasVolumeMount(dep, "loki-read-secret-token"), "volume mount should be present")

		value, ok := envValue(dep, "LOKI_READ_TOKEN_FILE")
		require.True(t, ok, "LOKI_READ_TOKEN_FILE env should be present")
		require.Equal(t, lokiReadSecretTokenMountPath+"/token", value)

		// The token value must never be copied into the Deployment spec.
		_, _, ok = envSecretRef(dep, "LOKI_READ_TOKEN")
		require.False(t, ok, "LOKI_READ_TOKEN env should not exist")
	})

	t.Run("token flag without endpoint leaves the Secret token unmounted", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:            "test-image",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "test-gateway",
			LokiReadTokenFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		_, _, ok := secretVolume(dep, "loki-read-secret-token")
		require.False(t, ok, "loki-read-secret-token volume should be absent without an endpoint")
	})

	t.Run("CA flag mounts the Secret CA and repoints LOKI_READ_CA_FILE", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:         "test-image",
			GatewayNamespace:     "openshift-ingress",
			GatewayName:          "test-gateway",
			LokiReadEndpoint:     "https://loki.example.svc:8080/api/logs/v1/application",
			LokiReadCAFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		secretName, item, ok := secretVolume(dep, "loki-read-secret-ca")
		require.True(t, ok, "loki-read-secret-ca volume should be present")
		require.Equal(t, LokiReadSecretName, secretName)
		require.Equal(t, LokiReadCASecretKey+":ca.pem", item)
		require.True(t, hasVolumeMount(dep, "loki-read-secret-ca"), "volume mount should be present")

		value, ok := envValue(dep, "LOKI_READ_CA_FILE")
		require.True(t, ok, "LOKI_READ_CA_FILE env should be present")
		require.Equal(t, lokiReadSecretCAMountPath+"/ca.pem", value)

		// The CA bundle must never be copied into the Deployment spec.
		_, _, ok = envSecretRef(dep, "LOKI_READ_CA")
		require.False(t, ok, "LOKI_READ_CA env should not exist")
	})

	t.Run("CA flag without endpoint leaves the Secret CA unmounted", func(t *testing.T) {
		dep := makeDeployment()
		params := PlatformParams{
			MaaSAPIImage:         "test-image",
			GatewayNamespace:     "openshift-ingress",
			GatewayName:          "test-gateway",
			LokiReadCAFromSecret: true,
		}

		require.NoError(t, patchMaaSAPIDeployment(logr.Discard(), dep, params))

		_, _, ok := secretVolume(dep, "loki-read-secret-ca")
		require.False(t, ok, "loki-read-secret-ca volume should be absent without an endpoint")
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
