/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"maps"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// kuadrantWasmPluginProbe re-detects the kuadrant-<gateway> WasmPlugin for gateways whose
// tenants rendered against its presence or absence. maas-controller may get WasmPlugins
// but not list or watch them, so one get per gateway on a timer stands in for a watch,
// and a gateway's tenants are enqueued only when its plugin appears or goes away.
type kuadrantWasmPluginProbe struct {
	tenants  *TenantReconciler
	interval time.Duration
	events   chan event.TypedGenericEvent[reconcile.Request]

	mu sync.Mutex
	// rendered holds, per gateway, whether a WasmPlugin existed when its tenants last rendered.
	rendered map[types.NamespacedName]bool
}

func newKuadrantWasmPluginProbe(r *TenantReconciler, interval time.Duration) *kuadrantWasmPluginProbe {
	return &kuadrantWasmPluginProbe{
		tenants:  r,
		interval: interval,
		events:   make(chan event.TypedGenericEvent[reconcile.Request]),
		rendered: map[types.NamespacedName]bool{},
	}
}

// observe records what a tenant on gateway rendered against: a WasmPlugin carrier, or the
// router fallback that a WasmPlugin appearing would replace. The watched EnvoyFilter
// carrier, and a WasmPlugin that could not be read, need no probe.
func (p *kuadrantWasmPluginProbe) observe(gateway types.NamespacedName, runRes *tenantreconcile.RunResult) {
	if p == nil || runRes == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case runRes.KuadrantWasmPlugin:
		p.rendered[gateway] = true
	case runRes.KuadrantRouterFallback && runRes.KuadrantDetectionWarning == "":
		p.rendered[gateway] = false
	default:
		delete(p.rendered, gateway)
	}
}

// Start probes every interval until ctx ends. The manager runs it only on the leader,
// like the tenant controller it feeds.
func (p *kuadrantWasmPluginProbe) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, req := range p.changed(ctx) {
				select {
				case p.events <- event.TypedGenericEvent[reconcile.Request]{Object: req}:
				case <-ctx.Done():
					return nil
				}
			}
		}
	}
}

// changed gets the WasmPlugin of every gateway a tenant rendered against and returns the
// tenants of gateways whose plugin appeared or went away since then.
func (p *kuadrantWasmPluginProbe) changed(ctx context.Context) []reconcile.Request {
	p.mu.Lock()
	rendered := maps.Clone(p.rendered)
	p.mu.Unlock()

	var requests []reconcile.Request
	for gateway, wasPresent := range rendered {
		present, known := p.present(ctx, gateway)
		if !known || present == wasPresent {
			continue
		}
		// The tenants' reconcile records the new state again; until then one change is
		// reported once.
		p.mu.Lock()
		p.rendered[gateway] = present
		p.mu.Unlock()
		requests = append(requests, p.tenants.tenantsUsingGateway(ctx, gateway.Namespace, gateway.Name)...)
	}
	return requests
}

func (p *kuadrantWasmPluginProbe) present(ctx context.Context, gateway types.NamespacedName) (present, known bool) {
	plugin := &unstructured.Unstructured{}
	plugin.SetGroupVersionKind(tenantreconcile.GVKWasmPlugin)
	key := types.NamespacedName{Namespace: gateway.Namespace, Name: tenantreconcile.KuadrantGatewayResourceName(gateway.Name)}
	err := p.tenants.Get(ctx, key, plugin)
	switch {
	case err == nil:
		return true, true
	case apierrors.IsNotFound(err), apimeta.IsNoMatchError(err):
		return false, true
	default:
		oteljson.FromContext(ctx).V(1).Info("cannot probe Kuadrant WasmPlugin", "wasmPlugin", key, "error", err)
		return false, false
	}
}
