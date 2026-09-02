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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// Gateway-readiness sentinels: a gateway missing any of these is skipped without
// requeue and re-triggered by the relevant watch.
var (
	ErrGatewayNoHostname = errors.New("gateway has no listener hostname")
	ErrNoModelRoute      = errors.New("no model HTTPRoute for gateway")
	ErrNoMaaSAPIRoute    = errors.New("no maas-api HTTPRoute for gateway")
	ErrNoSubscription    = errors.New("no subscription for model route")
)

// gatherGatewayInput reads the cluster state a gateway's enforcement artifacts are
// generated from. This is the cluster-side half; the generators are pure.
func (r *GatewayEnforcementReconciler) gatherGatewayInput(ctx context.Context, gw types.NamespacedName) (GatewayEnforcementInput, error) {
	in := GatewayEnforcementInput{}

	hostname, err := r.gatewayHostname(ctx, gw)
	if err != nil {
		return in, err
	}
	in.Hostname = hostname

	modelRoute, maasAPIRoute, err := r.gatewayRoutes(ctx, gw)
	if err != nil {
		return in, err
	}
	in.ModelMatches = httpRouteMatches(modelRoute)
	in.MaasAPIMatches = httpRouteMatches(maasAPIRoute)

	modelRouteScope := qualifiedName(modelRoute.Namespace, modelRoute.Name)
	maasAPIScope := qualifiedName(maasAPIRoute.Namespace, maasAPIRoute.Name)

	in.ModelRouteScope = modelRouteScope
	in.ModelSource = "tokenratelimitpolicy.kuadrant.io:" + modelRouteScope
	subs, err := r.subscriptionsForRoute(ctx, modelRoute)
	if err != nil {
		return in, err
	}
	for _, sm := range subs {
		limit := sm.ref.TokenRateLimits[0]
		limitName := strings.ReplaceAll(qualifiedName(sm.sub.Namespace, sm.sub.Name), "/", "-") + "-" + sm.ref.Name + "-tokens"
		in.Subscriptions = append(in.Subscriptions, SubscriptionBinding{
			SubscriptionKey: qualifiedName(sm.sub.Namespace, sm.sub.Name) + "@" + qualifiedName(sm.ref.Namespace, sm.ref.Name),
			TokenLimitKey:   descriptorKey(limitName),
			LimitName:       limitName,
			MaxValue:        limit.Limit,
			Window:          limit.Window,
		})
	}
	slices.SortFunc(in.Subscriptions, func(a, b SubscriptionBinding) int {
		return cmp.Compare(a.LimitName, b.LimitName)
	})

	in.DenyAll = SubscriptionBinding{
		RouteScope:    maasAPIScope,
		Source:        "tokenratelimitpolicy.kuadrant.io:" + maasAPIScope,
		TokenLimitKey: descriptorKey("deny-all-by-default"),
		LimitName:     "deny-all-by-default",
		MaxValue:      0,
		Window:        "1m",
	}

	// One AuthConfig per gateway; its host is the auth scope, so it only needs to be stable.
	in.AuthConfigHost = hashHex(qualifiedName(gw.Namespace, gw.Name))
	in.AuthSource = "authpolicy.kuadrant.io:" + qualifiedName(gw.Namespace, gw.Name) + "-maas-auth"

	rules, err := r.gatherAuthRules(ctx, gw)
	if err != nil {
		return in, err
	}
	in.AuthRules = rules

	return in, nil
}

// gatewayHostname returns the gateway's first listener hostname.
func (r *GatewayEnforcementReconciler) gatewayHostname(ctx context.Context, gw types.NamespacedName) (string, error) {
	g := &gatewayapiv1.Gateway{}
	if err := r.Get(ctx, gw, g); err != nil {
		return "", fmt.Errorf("get gateway %s: %w", gw, err)
	}
	for _, l := range g.Spec.Listeners {
		if l.Hostname != nil && *l.Hostname != "" {
			return string(*l.Hostname), nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrGatewayNoHostname, gw)
}

// gatewayRoutes splits the gateway's routes into the model route and the maas-api
// route (named "maas-api-route*"; the rest is the model).
func (r *GatewayEnforcementReconciler) gatewayRoutes(ctx context.Context, gw types.NamespacedName) (model, maasAPI *gatewayapiv1.HTTPRoute, err error) {
	var routes gatewayapiv1.HTTPRouteList
	if err := r.List(ctx, &routes); err != nil {
		return nil, nil, fmt.Errorf("list httproutes: %w", err)
	}
	for i := range routes.Items {
		rt := &routes.Items[i]
		if !routeTargetsGateway(rt, gw) {
			continue
		}
		if strings.HasPrefix(rt.Name, "maas-api-route") {
			maasAPI = rt
		} else {
			model = rt
		}
	}
	if model == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoModelRoute, gw)
	}
	if maasAPI == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoMaaSAPIRoute, gw)
	}
	return model, maasAPI, nil
}

// routeTargetsGateway reports whether an HTTPRoute parent-refs the given gateway.
func routeTargetsGateway(rt *gatewayapiv1.HTTPRoute, gw types.NamespacedName) bool {
	for _, p := range rt.Spec.ParentRefs {
		if !parentRefTargetsGateway(p) || string(p.Name) != gw.Name {
			continue
		}
		ns := rt.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		if ns == gw.Namespace {
			return true
		}
	}
	return false
}

// httpRouteMatches flattens an HTTPRoute's rule matches into RouteMatch entries.
func httpRouteMatches(rt *gatewayapiv1.HTTPRoute) []RouteMatch {
	var out []RouteMatch
	for _, rule := range rt.Spec.Rules {
		for _, m := range rule.Matches {
			rm := RouteMatch{}
			if m.Path != nil {
				if m.Path.Type != nil {
					rm.PathType = string(*m.Path.Type)
				}
				if m.Path.Value != nil {
					rm.PathValue = *m.Path.Value
				}
			}
			for _, h := range m.Headers {
				rm.HeaderName = string(h.Name)
				rm.HeaderValue = h.Value
				break // MaaS routes use at most one header match
			}
			out = append(out, rm)
		}
	}
	return out
}

// subModelRef pairs a subscription with the model ref that resolves to a route.
type subModelRef struct {
	sub *maasv1alpha1.MaaSSubscription
	ref *maasv1alpha1.ModelSubscriptionRef
}

// subscriptionsForRoute finds every subscription+model ref whose model resolves to
// the given model HTTPRoute.
func (r *GatewayEnforcementReconciler) subscriptionsForRoute(ctx context.Context, modelRoute *gatewayapiv1.HTTPRoute) ([]subModelRef, error) {
	log := ctrl.LoggerFrom(ctx)
	var subs maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subs); err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	var out []subModelRef
	for i := range subs.Items {
		sub := &subs.Items[i]
		for j := range sub.Spec.ModelRefs {
			ref := &sub.Spec.ModelRefs[j]
			routeName, routeNS, err := findHTTPRouteForModel(ctx, r.Client, ref.Namespace, ref.Name)
			if err != nil {
				log.V(1).Info("skipping model ref with no resolvable route",
					"model", qualifiedName(ref.Namespace, ref.Name), "error", err.Error())
				continue
			}
			if routeName == modelRoute.Name && routeNS == modelRoute.Namespace && len(ref.TokenRateLimits) > 0 {
				out = append(out, subModelRef{sub: sub, ref: ref})
			}
		}
	}
	log.V(1).Info("subscriptionsForRoute", "subscriptions", len(subs.Items),
		"matched", len(out), "targetRoute", qualifiedName(modelRoute.Namespace, modelRoute.Name))
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSubscription, qualifiedName(modelRoute.Namespace, modelRoute.Name))
	}
	return out, nil
}

// gatherAuthRules builds the auth rule sections via the existing AuthPolicy
// builder, keeping the auth logic single-sourced. Model access is decided at
// request time by the subscription-info callout, so no allowlist is injected here.
func (r *GatewayEnforcementReconciler) gatherAuthRules(ctx context.Context, gw types.NamespacedName) (map[string]any, error) {
	tenantID := strings.TrimSuffix(strings.TrimPrefix(gw.Name, "maas-"), "-gateway")
	if tenantID == "default" {
		tenantID = "" // the default tenant uses the bare maas-api service name
	}
	ap := &MaaSAuthPolicyReconciler{
		Client:           r.Client,
		InfraNamespace:   r.InfraNamespace,
		ClusterAudience:  r.ClusterAudience,
		MetadataCacheTTL: r.MetadataCacheTTL,
		AuthzCacheTTL:    r.AuthzCacheTTL,
	}
	xAPIKeyEnabled := ap.discoverXAPIKeyNeeded(ctx, ctrl.LoggerFrom(ctx))
	spec := ap.buildGatewayAuthPolicySpec(nil, xAPIKeyEnabled, tenantID, tenantID, gw.Namespace, gw.Name)
	rules, ok := authPolicyRules(spec)
	if !ok {
		return nil, fmt.Errorf("could not extract auth rules for gateway %s", gw)
	}
	return rules, nil
}

// descriptorKey builds a Limitador descriptor key body from a limit name: the name
// with separators normalized, plus a short stable hash suffix.
func descriptorKey(limitName string) string {
	body := strings.ReplaceAll(limitName, "-", "_")
	h := sha256.Sum256([]byte(limitName))
	return fmt.Sprintf("%s__%s", body, hex.EncodeToString(h[:])[:8])
}

// hashHex is a stable hex hash, used for the per-gateway AuthConfig host.
func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
