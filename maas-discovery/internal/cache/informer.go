package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/gateway"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/types"
)

const rebuildDebounce = 100 * time.Millisecond

var (
	aiTenantGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "aitenants",
	}
	gatewayGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
	routeGVR = schema.GroupVersionResource{
		Group:    "route.openshift.io",
		Version:  "v1",
		Resource: "routes",
	}
)

// InformerCache implements TenantCache backed by dynamic informer watches
// on AITenant and Gateway CRs. It rebuilds the in-memory tenant map whenever
// a watched resource changes.
type InformerCache struct {
	log              *slog.Logger
	tenantNamespace  string
	gatewayNamespace string
	restConfig       *rest.Config

	rebuildMu sync.Mutex
	mu        sync.RWMutex
	tenants   []types.TenantInfo
	synced    atomic.Bool
}

// InformerCacheOptions configures an InformerCache.
type InformerCacheOptions struct {
	RestConfig       *rest.Config
	TenantNamespace  string
	GatewayNamespace string
	Log              *slog.Logger
}

// NewInformerCache creates an InformerCache. Call Start to begin watching.
func NewInformerCache(opts InformerCacheOptions) (*InformerCache, error) {
	if opts.RestConfig == nil {
		return nil, errors.New("rest config is required")
	}
	if opts.TenantNamespace == "" {
		return nil, errors.New("tenant namespace is required")
	}
	if opts.GatewayNamespace == "" {
		return nil, errors.New("gateway namespace is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &InformerCache{
		log:              log,
		tenantNamespace:  opts.TenantNamespace,
		gatewayNamespace: opts.GatewayNamespace,
		restConfig:       opts.RestConfig,
	}, nil
}

// Start sets up informer watches, waits for the initial sync, performs the
// first rebuild, and returns. Informers continue running in the background
// until ctx is cancelled. Returns an error if setup or initial sync fails.
func (ic *InformerCache) Start(ctx context.Context) error {
	dynamicClient, err := dynamic.NewForConfig(ic.restConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	tenantFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, ic.tenantNamespace, nil,
	)
	gatewayFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, ic.gatewayNamespace, nil,
	)

	tenantInformer := tenantFactory.ForResource(aiTenantGVR).Informer()
	gatewayInformer := gatewayFactory.ForResource(gatewayGVR).Informer()

	// Route informer (OpenShift only) — provides external hostnames for gateways
	// whose status.addresses only contain internal service names.
	var routeInformer k8scache.SharedIndexInformer
	routesAvailable := routeAPIAvailable(ic.restConfig)
	if routesAvailable {
		routeFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			dynamicClient, 0, ic.gatewayNamespace, nil,
		)
		routeInformer = routeFactory.ForResource(routeGVR).Informer()
	}

	rebuildCh := make(chan struct{}, 1)
	triggerRebuild := func() {
		select {
		case rebuildCh <- struct{}{}:
		default:
		}
	}

	handler := k8scache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { triggerRebuild() },
		UpdateFunc: func(_, _ any) { triggerRebuild() },
		DeleteFunc: func(_ any) { triggerRebuild() },
	}

	if _, err := tenantInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("adding AITenant event handler: %w", err)
	}
	if _, err := gatewayInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("adding Gateway event handler: %w", err)
	}
	if routeInformer != nil {
		if _, err := routeInformer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("adding Route event handler: %w", err)
		}
	}

	ic.log.Info("starting informer watches",
		"tenantNamespace", ic.tenantNamespace,
		"gatewayNamespace", ic.gatewayNamespace,
		"routesAvailable", routesAvailable)

	stopCh := ctx.Done()
	tenantFactory.Start(stopCh)
	gatewayFactory.Start(stopCh)
	if routeInformer != nil {
		go routeInformer.Run(stopCh)
	}

	tenantSynced := tenantFactory.WaitForCacheSync(stopCh)
	gatewaySynced := gatewayFactory.WaitForCacheSync(stopCh)

	for gvr, ok := range tenantSynced {
		if !ok {
			return fmt.Errorf("informer sync failed for %s", gvr.String())
		}
	}
	for gvr, ok := range gatewaySynced {
		if !ok {
			return fmt.Errorf("informer sync failed for %s", gvr.String())
		}
	}

	if routeInformer != nil {
		if !k8scache.WaitForCacheSync(stopCh, routeInformer.HasSynced) {
			ic.log.Warn("Route informer sync failed, continuing without Route data")
			routeInformer = nil
		}
	}

	// Drain any events queued during initial list before the authoritative rebuild.
	drainChannel(rebuildCh)

	ic.rebuildFromInformers(tenantInformer, gatewayInformer, routeInformer)
	ic.synced.Store(true)
	ic.log.Info("informer cache synced and ready")

	// Debounced rebuild loop — coalesces bursts of events into a single rebuild.
	go func() {
		for {
			select {
			case <-rebuildCh:
				t := time.NewTimer(rebuildDebounce)
				drainLoop(ctx, rebuildCh, t)
				ic.rebuildFromInformers(tenantInformer, gatewayInformer, routeInformer)
			case <-ctx.Done():
				return
			}
		}
	}()

	return nil
}

func drainChannel(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainLoop(ctx context.Context, ch <-chan struct{}, t *time.Timer) {
	for {
		select {
		case <-ch:
			if !t.Stop() {
				<-t.C
			}
			t.Reset(rebuildDebounce)
		case <-t.C:
			return
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// List returns the current tenant list.
func (ic *InformerCache) List() []types.TenantInfo {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return ic.tenants
}

// Synced returns true after the initial informer sync and first rebuild.
func (ic *InformerCache) Synced() bool {
	return ic.synced.Load()
}

// rebuildFromInformers reads the current state from the informer stores and rebuilds
// the in-memory tenant list. Serialized by rebuildMu to prevent stale data from a
// slower concurrent rebuild overwriting a newer one.
func (ic *InformerCache) rebuildFromInformers(tenantInformer, gatewayInformer, routeInformer k8scache.SharedIndexInformer) {
	ic.rebuildMu.Lock()
	defer ic.rebuildMu.Unlock()

	tenantObjs := tenantInformer.GetStore().List()
	gatewayObjs := gatewayInformer.GetStore().List()

	var tenants []unstructured.Unstructured
	for _, obj := range tenantObjs {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			tenants = append(tenants, *u)
		}
	}

	var gateways []unstructured.Unstructured
	for _, obj := range gatewayObjs {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			gateways = append(gateways, *u)
		}
	}

	var routes []unstructured.Unstructured
	if routeInformer != nil {
		for _, obj := range routeInformer.GetStore().List() {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				routes = append(routes, *u)
			}
		}
	}

	result := BuildTenantInfos(tenants, gateways, routes, ic.gatewayNamespace, ic.log)

	ic.mu.Lock()
	ic.tenants = result
	ic.mu.Unlock()

	ic.log.Debug("tenant cache rebuilt", "count", len(result))
}

// BuildTenantInfos builds the tenant info list from raw AITenant and Gateway objects.
// Exported for testing.
func BuildTenantInfos(
	tenants []unstructured.Unstructured,
	gateways []unstructured.Unstructured,
	routes []unstructured.Unstructured,
	gatewayNamespace string,
	log *slog.Logger,
) []types.TenantInfo {
	gwByName := make(map[string]*unstructured.Unstructured, len(gateways))
	for i := range gateways {
		gwByName[gateways[i].GetName()] = &gateways[i]
	}

	routeHosts := gateway.BuildRouteHostMap(routes)

	result := make([]types.TenantInfo, 0, len(tenants))
	for i := range tenants {
		t := &tenants[i]
		name := t.GetName()
		gwName := resolveGatewayName(t)

		info := types.TenantInfo{
			Name: name,
			Gateway: types.GatewayMetadata{
				Name:      gwName,
				Namespace: gatewayNamespace,
			},
		}

		gw, found := gwByName[gwName]
		if !found {
			log.Warn("gateway not found for tenant",
				"tenant", name, "gateway", gwName, "gatewayNamespace", gatewayNamespace)
			result = append(result, info)
			continue
		}

		meta, err := gateway.ExtractMetadata(gw.Object, gwName, gatewayNamespace, routeHosts)
		if err != nil {
			log.Warn("gateway metadata extraction failed, returning partial data",
				"tenant", name, "gateway", gwName, "error", err)
			result = append(result, info)
			continue
		}

		info.Gateway = *meta
		result = append(result, info)
	}
	return result
}

// resolveGatewayName returns the gateway name for an AITenant.
// Uses spec.gateway.name if set, otherwise falls back to the AITenant name.
func resolveGatewayName(tenant *unstructured.Unstructured) string {
	spec, ok := tenant.Object["spec"].(map[string]any)
	if !ok {
		return tenant.GetName()
	}
	gw, ok := spec["gateway"].(map[string]any)
	if !ok {
		return tenant.GetName()
	}
	name, ok := gw["name"].(string)
	if !ok || name == "" {
		return tenant.GetName()
	}
	return name
}

// routeAPIAvailable checks whether the route.openshift.io/v1 API group is
// registered on the cluster. Returns false on vanilla Kubernetes.
func routeAPIAvailable(cfg *rest.Config) bool {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return false
	}
	_, resources, err := dc.ServerGroupsAndResources()
	if err != nil {
		return false
	}
	for _, rl := range resources {
		if rl.GroupVersion == "route.openshift.io/v1" {
			for _, r := range rl.APIResources {
				if r.Name == "routes" {
					return true
				}
			}
		}
	}
	return false
}
