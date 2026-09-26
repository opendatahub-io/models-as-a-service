package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	authPolicyGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "maasauthpolicies",
	}
	subscriptionGVR = schema.GroupVersionResource{
		Group:    "maas.opendatahub.io",
		Version:  "v1alpha1",
		Resource: "maassubscriptions",
	}
)

const (
	defaultTenantNS     = "models-as-a-service"
	subscriptionPhaseOK = "Active"
	subscriptionPhaseDG = "Degraded"
)

// InformerCache implements TenantCache backed by dynamic informer watches
// on AITenant and Gateway CRs. It rebuilds the in-memory tenant map whenever
// a watched resource changes.
type InformerCache struct {
	log              *slog.Logger
	tenantNamespace  string
	gatewayNamespace string
	restConfig       *rest.Config

	metadataRebuildMu    sync.Mutex
	entitlementRebuildMu sync.Mutex
	mu                   sync.RWMutex
	tenants              []types.TenantInfo
	subjectsByTenant     map[string]map[string]struct{}
	tenantsBySubject     map[string]map[string]struct{}
	dirtyTenants         map[string]struct{}
	synced               atomic.Bool
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
		subjectsByTenant: make(map[string]map[string]struct{}),
		tenantsBySubject: make(map[string]map[string]struct{}),
		dirtyTenants:     make(map[string]struct{}),
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
	entitlementFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient, 0, "", nil,
	)
	authPolicyInformer := entitlementFactory.ForResource(authPolicyGVR).Informer()
	subscriptionInformer := entitlementFactory.ForResource(subscriptionGVR).Informer()

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
	entitlementRebuildCh := make(chan struct{}, 1)
	triggerRebuild := func() {
		select {
		case rebuildCh <- struct{}{}:
		default:
		}
	}
	triggerEntitlementRebuild := func() {
		select {
		case entitlementRebuildCh <- struct{}{}:
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

	entitlementHandler := k8scache.ResourceEventHandlerFuncs{
		AddFunc: func(_ any) {
			ic.markAllTenantsDirty()
			triggerEntitlementRebuild()
		},
		UpdateFunc: func(_, _ any) {
			ic.markAllTenantsDirty()
			triggerEntitlementRebuild()
		},
		DeleteFunc: func(_ any) {
			ic.markAllTenantsDirty()
			triggerEntitlementRebuild()
		},
	}

	if _, err := authPolicyInformer.AddEventHandler(entitlementHandler); err != nil {
		return fmt.Errorf("adding MaaSAuthPolicy event handler: %w", err)
	}
	if _, err := subscriptionInformer.AddEventHandler(entitlementHandler); err != nil {
		return fmt.Errorf("adding MaaSSubscription event handler: %w", err)
	}

	ic.log.Info("starting informer watches",
		"tenantNamespace", ic.tenantNamespace,
		"gatewayNamespace", ic.gatewayNamespace,
		"routesAvailable", routesAvailable)

	stopCh := ctx.Done()
	tenantFactory.Start(stopCh)
	gatewayFactory.Start(stopCh)
	entitlementFactory.Start(stopCh)
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
	if !k8scache.WaitForCacheSync(stopCh, authPolicyInformer.HasSynced, subscriptionInformer.HasSynced) {
		return errors.New("informer sync failed for entitlement informers")
	}

	if routeInformer != nil {
		if !k8scache.WaitForCacheSync(stopCh, routeInformer.HasSynced) {
			ic.log.Warn("Route informer sync failed, continuing without Route data")
			routeInformer = nil
		}
	}

	// Drain any events queued during initial list before the authoritative rebuild.
	drainChannel(rebuildCh)
	drainChannel(entitlementRebuildCh)

	ic.rebuildFromInformers(tenantInformer, gatewayInformer, routeInformer)
	ic.markAllTenantsDirty()
	ic.rebuildDirtyEntitlements(tenantInformer, authPolicyInformer, subscriptionInformer)
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
				ic.markAllTenantsDirty()
				triggerEntitlementRebuild()
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case <-entitlementRebuildCh:
				t := time.NewTimer(rebuildDebounce)
				drainLoop(ctx, entitlementRebuildCh, t)
				ic.rebuildDirtyEntitlements(tenantInformer, authPolicyInformer, subscriptionInformer)
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

// ListForSubjects returns tenants visible to at least one caller subject.
func (ic *InformerCache) ListForSubjects(username string, groups []string) []types.TenantInfo {
	visibleByTenant := make(map[string]struct{})

	ic.mu.RLock()
	if username != "" {
		for tenant := range ic.tenantsBySubject[userSubjectKey(username)] {
			visibleByTenant[tenant] = struct{}{}
		}
	}
	for _, group := range groups {
		for tenant := range ic.tenantsBySubject[groupSubjectKey(group)] {
			visibleByTenant[tenant] = struct{}{}
		}
	}
	tenantList := ic.tenants
	ic.mu.RUnlock()

	if len(visibleByTenant) == 0 {
		return nil
	}

	result := make([]types.TenantInfo, 0, len(visibleByTenant))
	for i := range tenantList {
		if _, ok := visibleByTenant[tenantList[i].Name]; ok {
			result = append(result, tenantList[i])
		}
	}
	return result
}

// Synced returns true after the initial informer sync and first rebuild.
func (ic *InformerCache) Synced() bool {
	return ic.synced.Load()
}

// rebuildFromInformers reads the current state from the informer stores and rebuilds
// the in-memory tenant list. Serialized by rebuildMu to prevent stale data from a
// slower concurrent rebuild overwriting a newer one.
func (ic *InformerCache) rebuildFromInformers(tenantInformer, gatewayInformer, routeInformer k8scache.SharedIndexInformer) {
	ic.metadataRebuildMu.Lock()
	defer ic.metadataRebuildMu.Unlock()

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

func (ic *InformerCache) markTenantDirty(tenantName string) {
	tenantName = strings.TrimSpace(tenantName)
	if tenantName == "" {
		return
	}
	ic.mu.Lock()
	ic.dirtyTenants[tenantName] = struct{}{}
	ic.mu.Unlock()
}

func (ic *InformerCache) markAllTenantsDirty() {
	ic.mu.Lock()
	for i := range ic.tenants {
		ic.dirtyTenants[ic.tenants[i].Name] = struct{}{}
	}
	ic.mu.Unlock()
}

func (ic *InformerCache) takeDirtyTenants() []string {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	if len(ic.dirtyTenants) == 0 {
		return nil
	}

	tenants := make([]string, 0, len(ic.dirtyTenants))
	for tenant := range ic.dirtyTenants {
		tenants = append(tenants, tenant)
		delete(ic.dirtyTenants, tenant)
	}
	return tenants
}

func (ic *InformerCache) rebuildDirtyEntitlements(tenantInformer, authPolicyInformer, subscriptionInformer k8scache.SharedIndexInformer) {
	ic.entitlementRebuildMu.Lock()
	defer ic.entitlementRebuildMu.Unlock()

	dirtyTenants := ic.takeDirtyTenants()
	if len(dirtyTenants) == 0 {
		return
	}

	policies := listUnstructured(authPolicyInformer.GetStore().List())
	subscriptions := listUnstructured(subscriptionInformer.GetStore().List())
	tenants := listUnstructured(tenantInformer.GetStore().List())
	namespaceToTenant := buildNamespaceTenantMap(tenants)

	for _, tenantName := range dirtyTenants {
		subjects := computeTenantSubjects(tenantName, policies, subscriptions, namespaceToTenant)
		ic.applyTenantSubjects(tenantName, subjects)
		ic.log.Debug("tenant entitlement recomputed",
			"tenant", tenantName,
			"subject_count", len(subjects),
		)
	}

	ic.log.Debug("entitlement cache rebuilt", "dirtyTenants", len(dirtyTenants))
}

func (ic *InformerCache) applyTenantSubjects(tenantName string, newSubjects map[string]struct{}) {
	ic.mu.Lock()
	defer ic.mu.Unlock()

	oldSubjects := ic.subjectsByTenant[tenantName]
	for subject := range oldSubjects {
		tenants := ic.tenantsBySubject[subject]
		delete(tenants, tenantName)
		if len(tenants) == 0 {
			delete(ic.tenantsBySubject, subject)
		}
	}

	if len(newSubjects) == 0 {
		delete(ic.subjectsByTenant, tenantName)
		return
	}

	ic.subjectsByTenant[tenantName] = newSubjects
	for subject := range newSubjects {
		tenants := ic.tenantsBySubject[subject]
		if tenants == nil {
			tenants = make(map[string]struct{})
			ic.tenantsBySubject[subject] = tenants
		}
		tenants[tenantName] = struct{}{}
	}
}

func listUnstructured(objs []any) []unstructured.Unstructured {
	result := make([]unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			result = append(result, *u)
		}
	}
	return result
}

func computeTenantSubjects(
	tenantName string,
	policies []unstructured.Unstructured,
	subscriptions []unstructured.Unstructured,
	namespaceToTenant map[string]string,
) map[string]struct{} {
	policySubjects := make(map[string]struct{})
	for i := range policies {
		if namespaceToTenant[policies[i].GetNamespace()] != tenantName {
			continue
		}
		spec, ok := policies[i].Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		subjects, ok := spec["subjects"].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range userSubjects(subjects["users"]) {
			policySubjects[key] = struct{}{}
		}
		for _, key := range groupSubjects(subjects["groups"]) {
			policySubjects[key] = struct{}{}
		}
	}

	out := make(map[string]struct{})
	for i := range subscriptions {
		if namespaceToTenant[subscriptions[i].GetNamespace()] != tenantName {
			continue
		}
		spec, ok := subscriptions[i].Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		if !subscriptionEligible(subscriptions[i].Object) {
			continue
		}
		owner, ok := spec["owner"].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range userSubjects(owner["users"]) {
			if _, ok := policySubjects[key]; ok {
				out[key] = struct{}{}
			}
		}
		for _, key := range groupSubjects(owner["groups"]) {
			if _, ok := policySubjects[key]; ok {
				out[key] = struct{}{}
			}
		}
	}

	return out
}

func subscriptionEligible(obj map[string]any) bool {
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return false
	}
	phase, _ := status["phase"].(string)
	phase = strings.TrimSpace(phase)
	return phase == subscriptionPhaseOK || phase == subscriptionPhaseDG
}

func buildNamespaceTenantMap(tenants []unstructured.Unstructured) map[string]string {
	out := make(map[string]string, len(tenants)+1)
	for i := range tenants {
		tenant := tenants[i]
		tenantName := strings.TrimSpace(tenant.GetName())
		if tenantName == "" {
			continue
		}

		nsName, _, _ := unstructured.NestedString(tenant.Object, "status", "tenantNamespace")
		nsName = strings.TrimSpace(nsName)
		if nsName == "" {
			continue
		}
		out[nsName] = tenantName
	}

	if _, ok := out[defaultTenantNS]; !ok {
		out[defaultTenantNS] = defaultTenantNS
	}

	return out
}

func userSubjects(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		user, ok := item.(string)
		if !ok {
			continue
		}
		key := userSubjectKey(user)
		if key != "" {
			out = append(out, key)
		}
	}
	return out
}

func groupSubjects(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		groupMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := groupMap["name"].(string)
		key := groupSubjectKey(name)
		if key != "" {
			out = append(out, key)
		}
	}
	return out
}

func userSubjectKey(username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	return "u:" + username
}

func groupSubjectKey(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
		return ""
	}
	return "g:" + group
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
