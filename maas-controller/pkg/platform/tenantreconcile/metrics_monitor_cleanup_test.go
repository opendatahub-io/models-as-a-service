package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func legacyControllerMonitor() *unstructured.Unstructured {
	m := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"endpoints": []any{map[string]any{"port": "metrics", "bearerTokenFile": legacyMetricsBearerTokenFile}},
		},
	}}
	m.SetGroupVersionKind(GVKServiceMonitor)
	m.SetNamespace("controller-ns")
	m.SetName(controllerMetricsMonitorName)
	m.SetUID("old-uid")
	m.SetResourceVersion("1")
	m.SetLabels(map[string]string{
		"app": "maas-controller", "monitoring.opendatahub.io/scrape": "true",
		"app.kubernetes.io/name": "maas-controller", "app.kubernetes.io/component": "models-as-a-service",
		"app.kubernetes.io/part-of": "models-as-a-service",
	})
	return m
}

func aigatewayOwnedLegacyControllerMonitor() *unstructured.Unstructured {
	m := legacyControllerMonitor()
	labels := m.GetLabels()
	labels["app.kubernetes.io/part-of"] = "aigateway"
	delete(labels, "app.kubernetes.io/name")
	delete(labels, "app.kubernetes.io/component")
	m.SetLabels(labels)
	m.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "components.platform.opendatahub.io/v1alpha1", Kind: "AIGateway",
		Name: "default-aigateway", UID: "aigateway-uid",
	}})
	m.Object["spec"].(map[string]any)["selector"] = map[string]any{
		"matchLabels": map[string]any{"app": "maas-controller", "control-plane": "maas-controller"},
	}
	m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["path"] = "/metrics"
	m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["scheme"] = "https"
	return m
}

func replacementControllerMonitor() *unstructured.Unstructured {
	m := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"namespaceSelector": map[string]any{"matchNames": []any{"controller-ns"}},
			"selector": map[string]any{"matchLabels": map[string]any{
				"control-plane": "maas-controller", "app": "maas-controller",
			}},
			"endpoints": []any{map[string]any{
				"port": "metrics", "scheme": "https",
				"authorization": map[string]any{
					"type": "Bearer", "credentials": map[string]any{"name": metricsScraperTokenName, "key": "token"},
				},
			}},
		},
	}}
	m.SetGroupVersionKind(GVKServiceMonitor)
	m.SetNamespace("app-ns")
	m.SetName(controllerMetricsMonitorName)
	m.SetLabels(map[string]string{"monitoring.opendatahub.io/scrape": "true"})
	m.SetOwnerReferences([]metav1.OwnerReference{legacyMetricsOwner()})
	return m
}

func monitorCleanupClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	scheme.AddKnownTypeWithName(GVKServiceMonitor, &unstructured.Unstructured{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestHasRenderedControllerMetricsMonitor(t *testing.T) {
	monitor := replacementControllerMonitor()
	assert.True(t, hasRenderedControllerMetricsMonitor([]unstructured.Unstructured{*monitor}, "app-ns"))
	assert.False(t, hasRenderedControllerMetricsMonitor([]unstructured.Unstructured{*monitor}, "other-ns"))
	assert.False(t, hasRenderedControllerMetricsMonitor(nil, "app-ns"), "xKS render has no ServiceMonitor")
}

func runMonitorCleanup(c client.Client, tenantID string) (string, error) {
	return cleanupLegacyControllerMetricsMonitor(context.Background(), c, c, metricsTestConfigUID,
		"app-ns", "controller-ns", tenantID, logr.Discard())
}

func TestCleanupLegacyControllerMetricsMonitorWaitsForReplacementAndToken(t *testing.T) {
	ctx := context.Background()
	c := monitorCleanupClient(t, legacyControllerMonitor())
	pending, err := runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.NotEmpty(t, pending)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "controller-ns", Name: controllerMetricsMonitorName}, legacyControllerMonitor()))

	require.NoError(t, c.Create(ctx, replacementControllerMonitor()))
	pending, err = runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.NotEmpty(t, pending)

	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: metricsScraperTokenName, Namespace: "app-ns", OwnerReferences: []metav1.OwnerReference{legacyMetricsOwner()},
		Annotations: map[string]string{corev1.ServiceAccountNameKey: "maas-metrics-scraper"},
	}, Type: corev1.SecretTypeServiceAccountToken}
	require.NoError(t, c.Create(ctx, token))
	pending, err = runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.NotEmpty(t, pending, "wait for service-account token controller to populate data")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(token), token))
	token.Data = map[string][]byte{corev1.ServiceAccountTokenKey: []byte("issued-token")}
	require.NoError(t, c.Update(ctx, token))
	pending, err = runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.Empty(t, pending)
	err = c.Get(ctx, client.ObjectKey{Namespace: "controller-ns", Name: controllerMetricsMonitorName}, legacyControllerMonitor())
	assert.True(t, apierrors.IsNotFound(err), "old monitor should be deleted: %v", err)
}

func TestCleanupLegacyControllerMetricsMonitorPreservesUnownedOrChangedMonitor(t *testing.T) {
	cases := map[string]func(*unstructured.Unstructured){
		"foreign owner": func(m *unstructured.Unstructured) {
			m.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Config", UID: types.UID("foreign")}})
		},
		"wrong labels": func(m *unstructured.Unstructured) {
			labels := m.GetLabels()
			delete(labels, "app.kubernetes.io/name")
			m.SetLabels(labels)
		},
		"new credentials": func(m *unstructured.Unstructured) {
			m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["bearerTokenFile"] = "other"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			old := legacyControllerMonitor()
			mutate(old)
			c := monitorCleanupClient(t, old)
			pending, err := runMonitorCleanup(c, "")
			require.NoError(t, err)
			assert.Empty(t, pending)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(old), legacyControllerMonitor()))
		})
	}
}

func TestCleanupLegacyControllerMetricsMonitorRequiresOwnedReplacement(t *testing.T) {
	old := legacyControllerMonitor()
	replacement := replacementControllerMonitor()
	replacement.SetOwnerReferences(nil)
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: metricsScraperTokenName, Namespace: "app-ns", OwnerReferences: []metav1.OwnerReference{legacyMetricsOwner()},
		Annotations: map[string]string{corev1.ServiceAccountNameKey: "maas-metrics-scraper"},
	}, Type: corev1.SecretTypeServiceAccountToken, Data: map[string][]byte{corev1.ServiceAccountTokenKey: []byte("issued-token")}}
	c := monitorCleanupClient(t, old, replacement, token)
	pending, err := runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.NotEmpty(t, pending)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(old), legacyControllerMonitor()))

	pending, err = runMonitorCleanup(c, "team")
	require.NoError(t, err)
	assert.Empty(t, pending, "other tenants cannot retire the shared monitor")
}

func TestCleanupLegacyControllerMetricsMonitorRetiresAIGatewayOwnedMaaSScrape(t *testing.T) {
	old := aigatewayOwnedLegacyControllerMonitor()
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: metricsScraperTokenName, Namespace: "app-ns", OwnerReferences: []metav1.OwnerReference{legacyMetricsOwner()},
		Annotations: map[string]string{corev1.ServiceAccountNameKey: "maas-metrics-scraper"},
	}, Type: corev1.SecretTypeServiceAccountToken, Data: map[string][]byte{corev1.ServiceAccountTokenKey: []byte("issued-token")}}
	c := monitorCleanupClient(t, old)
	pending, err := runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.NotEmpty(t, pending, "wait for the replacement before deleting an AIGateway-owned monitor")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(old), legacyControllerMonitor()))
	require.NoError(t, c.Create(context.Background(), replacementControllerMonitor()))
	require.NoError(t, c.Create(context.Background(), token))
	pending, err = runMonitorCleanup(c, "")
	require.NoError(t, err)
	assert.Empty(t, pending)
	err = c.Get(context.Background(), client.ObjectKeyFromObject(old), legacyControllerMonitor())
	assert.True(t, apierrors.IsNotFound(err), "legacy AIGateway-owned monitor should be deleted: %v", err)
}

func TestCleanupLegacyControllerMetricsMonitorPreservesOtherAIGatewayResources(t *testing.T) {
	cases := map[string]func(*unstructured.Unstructured){
		"scrape label absent": func(m *unstructured.Unstructured) {
			labels := m.GetLabels()
			delete(labels, "monitoring.opendatahub.io/scrape")
			m.SetLabels(labels)
		},
		"other owner": func(m *unstructured.Unstructured) {
			owners := m.GetOwnerReferences()
			owners[0].Name = "other-aigateway"
			m.SetOwnerReferences(owners)
		},
		"other owner group": func(m *unstructured.Unstructured) {
			owners := m.GetOwnerReferences()
			owners[0].APIVersion = "other.example.com/v1alpha1"
			m.SetOwnerReferences(owners)
		},
		"missing owner UID": func(m *unstructured.Unstructured) {
			owners := m.GetOwnerReferences()
			owners[0].UID = ""
			m.SetOwnerReferences(owners)
		},
		"additional owner": func(m *unstructured.Unstructured) {
			m.SetOwnerReferences(append(m.GetOwnerReferences(), legacyMetricsOwner()))
		},
		"other service selector": func(m *unstructured.Unstructured) {
			m.Object["spec"].(map[string]any)["selector"].(map[string]any)["matchLabels"].(map[string]any)["app"] = "other"
		},
		"other endpoint path": func(m *unstructured.Unstructured) {
			m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["path"] = "/other"
		},
		"other endpoint scheme": func(m *unstructured.Unstructured) {
			m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["scheme"] = "http"
		},
		"new credentials": func(m *unstructured.Unstructured) {
			delete(m.Object["spec"].(map[string]any)["endpoints"].([]any)[0].(map[string]any), "bearerTokenFile")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			old := aigatewayOwnedLegacyControllerMonitor()
			mutate(old)
			assert.False(t, isLegacyControllerMetricsMonitor(old, metricsTestConfigUID))
			c := monitorCleanupClient(t, old, replacementControllerMonitor())
			pending, err := runMonitorCleanup(c, "")
			require.NoError(t, err)
			assert.Empty(t, pending)
			require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(old), legacyControllerMonitor()))
		})
	}
}
