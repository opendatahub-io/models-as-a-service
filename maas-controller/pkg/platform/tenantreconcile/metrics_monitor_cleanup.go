package tenantreconcile

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	controllerMetricsMonitorName = "maas-controller-metrics"
	metricsScraperTokenName      = "maas-metrics-scraper-token"
	legacyMetricsBearerTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

func hasRenderedControllerMetricsMonitor(resources []unstructured.Unstructured, appNamespace string) bool {
	for i := range resources {
		if resources[i].GroupVersionKind() == GVKServiceMonitor &&
			resources[i].GetName() == controllerMetricsMonitorName && resources[i].GetNamespace() == appNamespace {
			return true
		}
	}
	return false
}

// cleanupLegacyControllerMetricsMonitor removes the old bootstrap monitor only
// after the default tenant has established its replacement and token Secret.
// Bootstrap kubectl apply gave the old monitor no owner reference. Older ODH
// installs may instead attach their AIGateway owner. In either case, verify the
// legacy scrape configuration before deleting the controller's old monitor.
func cleanupLegacyControllerMetricsMonitor(ctx context.Context, c client.Client, reader client.Reader,
	configUID types.UID, appNamespace, controllerNamespace, tenantID string, log logr.Logger) (string, error) {
	if tenantID != "" || appNamespace == controllerNamespace || controllerNamespace == "" {
		return "", nil
	}
	old := &unstructured.Unstructured{}
	old.SetGroupVersionKind(GVKServiceMonitor)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: controllerNamespace, Name: controllerMetricsMonitorName}, old); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get old ServiceMonitor: %w", err)
	}
	if !isLegacyControllerMetricsMonitor(old, configUID) {
		log.V(1).Info("Preserving controller-namespace ServiceMonitor without known legacy MaaS scrape configuration",
			"namespace", controllerNamespace, "name", old.GetName())
		return "", nil
	}

	const pending = "waiting for replacement controller metrics ServiceMonitor and scraper token"
	replacement := &unstructured.Unstructured{}
	replacement.SetGroupVersionKind(GVKServiceMonitor)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: controllerMetricsMonitorName}, replacement); err != nil {
		if apierrors.IsNotFound(err) {
			return pending, nil
		}
		return "", fmt.Errorf("get replacement ServiceMonitor: %w", err)
	}
	if !isReadyControllerMetricsReplacement(replacement, configUID, controllerNamespace) {
		return pending, nil
	}
	token := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: metricsScraperTokenName}, token); err != nil {
		if apierrors.IsNotFound(err) {
			return pending, nil
		}
		return "", fmt.Errorf("get metrics scraper token Secret: %w", err)
	}
	if !isOwnedByConfigUID(token, configUID) || token.Type != corev1.SecretTypeServiceAccountToken ||
		token.Annotations[corev1.ServiceAccountNameKey] != "maas-metrics-scraper" ||
		len(token.Data[corev1.ServiceAccountTokenKey]) == 0 {
		return pending, nil
	}

	options, err := validatedDeleteOptions(old)
	if err != nil {
		return "", fmt.Errorf("prepare old ServiceMonitor deletion: %w", err)
	}
	if err := c.Delete(ctx, old, options...); err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("delete old ServiceMonitor: %w", err)
	}
	log.Info("Deleted legacy controller-namespace metrics ServiceMonitor", "namespace", controllerNamespace)
	return "", nil
}

func isLegacyControllerMetricsMonitor(monitor *unstructured.Unstructured, configUID types.UID) bool {
	if monitor.GetName() != controllerMetricsMonitorName {
		return false
	}
	labels := monitor.GetLabels()
	if labels["app"] != "maas-controller" || labels["monitoring.opendatahub.io/scrape"] != "true" {
		return false
	}
	owners := monitor.GetOwnerReferences()
	switch labels["app.kubernetes.io/part-of"] {
	case "models-as-a-service":
		if labels["app.kubernetes.io/name"] != "maas-controller" ||
			labels["app.kubernetes.io/component"] != "models-as-a-service" {
			return false
		}
		for _, owner := range owners {
			if owner.Kind != "Config" || owner.UID != configUID {
				return false
			}
		}
	case "aigateway":
		if len(owners) != 1 || owners[0].Kind != "AIGateway" || owners[0].Name != "default-aigateway" ||
			owners[0].UID == "" || owners[0].APIVersion != "components.platform.opendatahub.io/v1alpha1" {
			return false
		}
		// AIGateway owns other monitors too. Only retire its old MaaS controller
		// scrape, identified by the service selector and HTTPS endpoint below.
		selector, found, err := unstructured.NestedStringMap(monitor.Object, "spec", "selector", "matchLabels")
		if err != nil || !found || len(selector) != 2 || selector["app"] != "maas-controller" ||
			selector["control-plane"] != "maas-controller" {
			return false
		}
	default:
		return false
	}
	endpoints, found, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		return false
	}
	endpoint, ok := endpoints[0].(map[string]any)
	if !ok || endpoint["bearerTokenFile"] != legacyMetricsBearerTokenFile || endpoint["port"] != "metrics" {
		return false
	}
	if labels["app.kubernetes.io/part-of"] == "aigateway" {
		return endpoint["path"] == "/metrics" && endpoint["scheme"] == "https"
	}
	return true
}

func isReadyControllerMetricsReplacement(monitor *unstructured.Unstructured, configUID types.UID, controllerNamespace string) bool {
	if !isOwnedByConfigUID(monitor, configUID) || monitor.GetLabels()["monitoring.opendatahub.io/scrape"] != "true" {
		return false
	}
	namespaces, found, err := unstructured.NestedStringSlice(monitor.Object, "spec", "namespaceSelector", "matchNames")
	if err != nil || !found || len(namespaces) != 1 || namespaces[0] != controllerNamespace {
		return false
	}
	selector, found, err := unstructured.NestedStringMap(monitor.Object, "spec", "selector", "matchLabels")
	if err != nil || !found || selector["control-plane"] != "maas-controller" || selector["app"] != "maas-controller" {
		return false
	}
	endpoints, found, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		return false
	}
	endpoint, ok := endpoints[0].(map[string]any)
	if !ok || endpoint["port"] != "metrics" || endpoint["scheme"] != "https" || endpoint["bearerTokenFile"] != nil {
		return false
	}
	authorizationType, found, err := unstructured.NestedString(endpoint, "authorization", "type")
	if err != nil || !found || authorizationType != "Bearer" {
		return false
	}
	credentials, found, err := unstructured.NestedStringMap(endpoint, "authorization", "credentials")
	return err == nil && found && credentials["name"] == metricsScraperTokenName && credentials["key"] == corev1.ServiceAccountTokenKey
}
