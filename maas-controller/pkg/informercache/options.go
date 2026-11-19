// Package informercache builds the controller-runtime cache.Options that scope
// informers to the namespaces and label selectors the controller actually needs.
package informercache

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// Params collects the namespace and mode values that determine cache scoping.
type Params struct {
	ControllerNamespace   string
	InfraNamespace        string
	MonitoringNamespace   string
	AITenantNamespace     string
	GatewayNamespace      string
	SubscriptionNamespace string
	EnableTenantDiscovery bool
	SubscriptionNSExists  bool
}

// BuildCacheOptions configures per-type informer scoping so the controller only
// caches the Kubernetes resources it actually needs. Without this,
// controller-runtime defaults to caching every instance of each watched type
// cluster-wide, which causes OOM on production clusters with hundreds of
// namespaces and thousands of Deployments/ConfigMaps/CRDs.
// See RHOAIENG-90683 / GH#1413.
func BuildCacheOptions(p Params) cache.Options {
	infraNsCfg := map[string]cache.Config{p.InfraNamespace: {}}

	// --- MaaSAuthPolicy / MaaSSubscription: scoped per mode ---
	subscriptionNsCfg := map[string]cache.Config{p.SubscriptionNamespace: {}}
	if p.EnableTenantDiscovery || !p.SubscriptionNSExists {
		subscriptionNsCfg = map[string]cache.Config{cache.AllNamespaces: {}}
	}

	// --- Deployment: controller + infra (teardown check) + gateway (payload-processing) ---
	deployNsCfg := UniqueNamespaceConfigs(p.ControllerNamespace, p.InfraNamespace, p.GatewayNamespace)

	// --- ConfigMap: monitoring (usage-logs) + aitenant (gateway claims) + infra (maas-parameters) + gateway (payload-processing-plugins) ---
	configMapNsCfg := UniqueNamespaceConfigs(p.MonitoringNamespace, p.AITenantNamespace, p.InfraNamespace, p.GatewayNamespace)

	// --- Pod: only in gateway namespace (IPP writer pod checks in pipeline.go) ---
	podNsCfg := map[string]cache.Config{p.GatewayNamespace: {}}

	// --- NetworkPolicy: all platform namespaces where NPs are managed ---
	networkPolicyNsCfg := UniqueNamespaceConfigs(
		p.MonitoringNamespace,
		p.InfraNamespace,
		p.SubscriptionNamespace,
		p.GatewayNamespace,
		p.ControllerNamespace,
	)

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			// MaasTenantConfig and Tenant stay cluster-wide: AITenant-created tenants
			// live in tenant-<name> namespaces that aren't known at startup.
			&maasv1alpha1.MaasTenantConfig{}: {},
			&maasv1alpha1.Tenant{}:           {},
			&maasv1alpha1.MaaSAuthPolicy{}:   {Namespaces: subscriptionNsCfg},
			&maasv1alpha1.MaaSSubscription{}: {Namespaces: subscriptionNsCfg},
			&corev1.Secret{}:                 {Namespaces: infraNsCfg},
			&appsv1.Deployment{}:             {Namespaces: deployNsCfg},
			&corev1.ConfigMap{}:              {Namespaces: configMapNsCfg},
			// Gateway is intentionally unscoped: tenant gateways live in
			// per-tenant namespaces not known at startup.
			&gatewayapiv1.Gateway{}: {},
			&netwv1.NetworkPolicy{}: {Namespaces: networkPolicyNsCfg},
			&rbacv1.ClusterRoleBinding{}: {
				Label: labels.SelectorFromSet(labels.Set{
					"app.kubernetes.io/managed-by": "maas-controller",
				}),
			},
			&corev1.Pod{}: {Namespaces: podNsCfg},
			// Namespace is intentionally unscoped: tenant namespace discovery watches
			// need to see label additions on any namespace.
		},
	}
}

// UniqueNamespaceConfigs builds a deduped Namespaces map, skipping empty strings.
func UniqueNamespaceConfigs(namespaces ...string) map[string]cache.Config {
	m := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		if ns != "" {
			m[ns] = cache.Config{}
		}
	}
	return m
}
