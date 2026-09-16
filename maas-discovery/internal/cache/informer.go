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

	ic.log.Info("starting informer watches",
		"tenantNamespace", ic.tenantNamespace,
		"gatewayNamespace", ic.gatewayNamespace)

	stopCh := ctx.Done()
	tenantFactory.Start(stopCh)
	gatewayFactory.Start(stopCh)

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

	// Drain any events queued during initial list before the authoritative rebuild.
	drainChannel(rebuildCh)

	ic.rebuildFromInformers(tenantInformer, gatewayInformer)
	ic.synced.Store(true)
	ic.log.Info("informer cache synced and ready")

	// Debounced rebuild loop — coalesces bursts of events into a single rebuild.
	go func() {
		for {
			select {
			case <-rebuildCh:
				t := time.NewTimer(rebuildDebounce)
				drainLoop(ctx, rebuildCh, t)
				ic.rebuildFromInformers(tenantInformer, gatewayInformer)
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
func (ic *InformerCache) rebuildFromInformers(tenantInformer, gatewayInformer k8scache.SharedIndexInformer) {
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

	result := BuildTenantInfos(tenants, gateways, ic.gatewayNamespace, ic.log)

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
	gatewayNamespace string,
	log *slog.Logger,
) []types.TenantInfo {
	gwByName := make(map[string]*unstructured.Unstructured, len(gateways))
	for i := range gateways {
		gwByName[gateways[i].GetName()] = &gateways[i]
	}

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

		meta, err := gateway.ExtractMetadata(gw.Object, gwName, gatewayNamespace)
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
