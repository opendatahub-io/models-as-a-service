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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// fieldOwner is the server-side-apply field manager, shared with the rest of the controller.
const fieldOwner = "maas-controller"

var (
	authConfigGVK = schema.GroupVersionKind{Group: "authorino.kuadrant.io", Version: "v1beta3", Kind: "AuthConfig"}
	limitadorGVK  = schema.GroupVersionKind{Group: "limitador.kuadrant.io", Version: "v1alpha1", Kind: "Limitador"}
)

// GatewayEnforcementReconciler compiles a MaaS gateway's enforcement config
// directly and drives the backends without Kuadrant: it gathers the gateway's
// routes, subscription limits, and auth rules, then applies the ext_proc
// ConfigMap, the Authorino AuthConfig, and the Limitador limits. Registered only
// when --enable-native-enforcement is set.
type GatewayEnforcementReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Infra namespace (maas-api), audience, and cache TTLs, for the generated AuthConfig.
	InfraNamespace   string
	ClusterAudience  string
	MetadataCacheTTL int64
	AuthzCacheTTL    int64

	// AuthConfigNamespace is where the AuthConfig is applied (Authorino is
	// cluster-wide, so any namespace works). Empty disables the apply.
	AuthConfigNamespace string

	// LimitadorNamespace/LimitadorName identify the shared Limitador CR whose
	// spec.limits this gateway's token limits merge into. Empty disables the apply.
	LimitadorNamespace string
	LimitadorName      string

	// ext_proc upstreams + Authorino TLS trust, written into the ConfigMap.
	AuthUpstream      string
	RatelimitUpstream string
	AuthCACertPath    string
	AuthSNI           string

	// MaaSAPIURL overrides the scheme://host:port of the AuthConfig maas-api
	// callouts, keeping each path. Empty keeps the buildGatewayAuthPolicySpec
	// default; set it when maas-api is served at a different scheme, host, or port.
	MaaSAPIURL string
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes,verbs=get;list;watch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maassubscriptions;maasmodelrefs,verbs=get;list;watch
//+kubebuilder:rbac:groups=authorino.kuadrant.io,resources=authconfigs,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=limitador.kuadrant.io,resources=limitadors,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

// Reconcile compiles and applies the enforcement config for the gateway named in
// the request. A gateway that is not yet a complete MaaS gateway is skipped
// without requeue; the HTTPRoute and subscription watches re-trigger it.
func (r *GatewayEnforcementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	in, err := r.gatherGatewayInput(ctx, req.NamespacedName)
	if err != nil {
		if isGatewayNotReady(err) {
			log.V(1).Info("gateway not ready for enforcement, skipping", "gateway", req.NamespacedName, "reason", err.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("gather gateway input: %w", err)
	}

	arts, err := generateGatewayArtifacts(in)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generate artifacts: %w", err)
	}

	rendered, err := r.renderPluginConfig(arts.PluginConfig)
	if err != nil {
		return ctrl.Result{}, err
	}
	cm := configMapFor(req.Name, req.Namespace, rendered)
	if err := r.Patch(ctx, cm, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply extproc ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	if r.MaaSAPIURL != "" {
		rewriteMetadataURLs(arts.AuthConfig, r.MaaSAPIURL)
	}
	if r.AuthConfigNamespace != "" {
		if err := r.applyAuthConfig(ctx, in.AuthConfigHost, arts.AuthConfig); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply AuthConfig: %w", err)
		}
	}
	if r.LimitadorName != "" {
		if err := r.applyLimits(ctx, arts); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply Limitador limits: %w", err)
		}
	}

	log.Info("reconciled gateway enforcement", "gateway", req.NamespacedName,
		"actionSets", len(arts.PluginConfig.ActionSets), "limits", len(arts.Limits))
	return ctrl.Result{}, nil
}

// isGatewayNotReady reports whether err means the gateway is not yet a complete
// MaaS gateway (missing hostname, routes, or subscriptions) and should be skipped
// without requeue rather than treated as a failure.
func isGatewayNotReady(err error) bool {
	return errors.Is(err, ErrGatewayNoHostname) ||
		errors.Is(err, ErrNoModelRoute) ||
		errors.Is(err, ErrNoMaaSAPIRoute) ||
		errors.Is(err, ErrNoSubscription)
}

// managedLabels marks the resources this reconciler owns.
func managedLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": fieldOwner,
		"app.kubernetes.io/part-of":    "native-enforcement",
	}
}

// applyAuthConfig server-side-applies the AuthConfig. Its name is the host hash;
// Authorino selects it by spec.hosts, which equals the pluginConfig auth scope.
func (r *GatewayEnforcementReconciler) applyAuthConfig(ctx context.Context, host string, spec map[string]any) error {
	ac := &unstructured.Unstructured{}
	ac.SetGroupVersionKind(authConfigGVK)
	ac.SetNamespace(r.AuthConfigNamespace)
	ac.SetName(host)
	ac.SetLabels(managedLabels())
	ac.Object["spec"] = spec
	return r.Patch(ctx, ac, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner))
}

// applyLimits merges this gateway's token limits into the shared Limitador CR,
// leaving other gateways' limits untouched. It skips the update when the merge is
// a no-op, so a redundant reconcile does not churn the shared CR.
func (r *GatewayEnforcementReconciler) applyLimits(ctx context.Context, arts GatewayArtifacts) error {
	lim := &unstructured.Unstructured{}
	lim.SetGroupVersionKind(limitadorGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.LimitadorNamespace, Name: r.LimitadorName}, lim); err != nil {
		return fmt.Errorf("get Limitador %s/%s: %w", r.LimitadorNamespace, r.LimitadorName, err)
	}

	log := ctrl.LoggerFrom(ctx)
	rawExisting, _, err := unstructured.NestedSlice(lim.Object, "spec", "limits")
	if err != nil {
		log.V(1).Info("reading existing Limitador limits, treating as empty", "error", err.Error())
	}
	var existing []LimitadorLimit
	if b, err := json.Marshal(rawExisting); err != nil {
		log.V(1).Info("marshaling existing Limitador limits, treating as empty", "error", err.Error())
	} else if err := json.Unmarshal(b, &existing); err != nil {
		log.V(1).Info("decoding existing Limitador limits, treating as empty", "error", err.Error())
	}

	mergedSlice, err := toUnstructuredSlice(mergeLimits(existing, arts.ownedScopes(), arts.Limits))
	if err != nil {
		return err
	}
	if unstructuredSlicesEqual(rawExisting, mergedSlice) {
		return nil
	}
	if err := unstructured.SetNestedSlice(lim.Object, mergedSlice, "spec", "limits"); err != nil {
		return err
	}
	if err := r.Update(ctx, lim); err != nil {
		return fmt.Errorf("update Limitador limits: %w", err)
	}
	return nil
}

// toUnstructuredSlice round-trips typed limits into the []any shape
// unstructured.SetNestedSlice requires.
func toUnstructuredSlice(limits []LimitadorLimit) ([]any, error) {
	b, err := json.Marshal(limits)
	if err != nil {
		return nil, err
	}
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// unstructuredSlicesEqual compares two limit slices by their canonical JSON.
func unstructuredSlicesEqual(a, b []any) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// extprocConfig is the ext_proc --kuadrant-config document.
type extprocConfig struct {
	Plugin    PluginConfiguration   `json:"plugin"`
	Upstreams map[string]string     `json:"upstreams"`
	TLS       map[string]extprocTLS `json:"tls,omitempty"`
}

// extprocTLS is the TLS trust for one upstream.
type extprocTLS struct {
	CACert string `json:"ca_cert"`
	SNI    string `json:"sni"`
}

// renderPluginConfig marshals the pluginConfig, upstreams, and TLS trust into the
// ext_proc config YAML. TLS is emitted only when a CA is configured.
func (r *GatewayEnforcementReconciler) renderPluginConfig(pc PluginConfiguration) (string, error) {
	doc := extprocConfig{
		Plugin: pc,
		Upstreams: map[string]string{
			authServiceEndpoint:      r.AuthUpstream,
			ratelimitServiceEndpoint: r.RatelimitUpstream,
		},
	}
	if r.AuthCACertPath != "" {
		doc.TLS = map[string]extprocTLS{authServiceEndpoint: {CACert: r.AuthCACertPath, SNI: r.AuthSNI}}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal kuadrant config: %w", err)
	}
	return string(out), nil
}

// configMapFor builds the extproc ConfigMap for a gateway.
func configMapFor(gwName, gwNamespace, kuadrantYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gwName + "-extproc",
			Namespace: gwNamespace,
			Labels:    managedLabels(),
		},
		Data: map[string]string{"kuadrant.yaml": kuadrantYAML},
	}
}

// SetupWithManager reconciles MaaS gateways (identified by the AITenant label) on
// their own spec changes and on changes to the HTTPRoutes and subscriptions that
// feed them. Generation predicates drop status-only churn.
func (r *GatewayEnforcementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	isMaaSGateway := predicate.NewPredicateFuncs(func(o client.Object) bool {
		_, ok := o.GetLabels()[aiGatewayTenantLabel]
		return ok
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayapiv1.Gateway{}, builder.WithPredicates(isMaaSGateway, predicate.GenerationChangedPredicate{})).
		Watches(&gatewayapiv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.gatewaysForRoute),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&maasv1alpha1.MaaSSubscription{}, handler.EnqueueRequestsFromMapFunc(r.gatewaysForSubscription)).
		Named("gateway-enforcement").
		Complete(r)
}

// gatewaysForRoute enqueues the gateways an HTTPRoute parent-refs.
func (r *GatewayEnforcementReconciler) gatewaysForRoute(_ context.Context, o client.Object) []reconcile.Request {
	rt, ok := o.(*gatewayapiv1.HTTPRoute)
	if !ok {
		return nil
	}
	var reqs []reconcile.Request
	for _, p := range rt.Spec.ParentRefs {
		if !parentRefTargetsGateway(p) {
			continue
		}
		ns := rt.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: string(p.Name)}})
	}
	return reqs
}

// gatewaysForSubscription enqueues every MaaS gateway, since a subscription change
// can shift limits on any of them. Gateways are few and subscription writes rare.
func (r *GatewayEnforcementReconciler) gatewaysForSubscription(ctx context.Context, _ client.Object) []reconcile.Request {
	var gws gatewayapiv1.GatewayList
	if err := r.List(ctx, &gws, client.HasLabels{aiGatewayTenantLabel}); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("listing gateways for subscription mapping", "error", err.Error())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(gws.Items))
	for i := range gws.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: gws.Items[i].Namespace, Name: gws.Items[i].Name,
		}})
	}
	return reqs
}
