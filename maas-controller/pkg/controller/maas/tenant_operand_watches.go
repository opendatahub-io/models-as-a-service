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
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const envoyFilterCRD = "envoyfilters.networking.istio.io"

var (
	gvkClusterRole    = rbacv1.SchemeGroupVersion.WithKind("ClusterRole")
	gvkServiceMonitor = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}

	// trackedSelector matches objects carrying the tenant-namespace tracking label.
	trackedSelector = labels.NewSelector().Add(mustRequirement(tenantreconcile.LabelTenantNamespace, selection.Exists))
)

func mustRequirement(key string, op selection.Operator, values ...string) labels.Requirement {
	req, err := labels.NewRequirement(key, op, values)
	if err != nil {
		panic(err)
	}
	return *req
}

// tenantOperand is a kind the tenant platform pipeline applies. Drift or deletion of an
// operand re-enqueues the MaasTenantConfig named in its tenant tracking labels.
type tenantOperand struct {
	gvk schema.GroupVersionKind
	// crd backs the kind; empty for built-in kinds.
	crd string
	// newObject returns the watch prototype. Kinds another controller always caches
	// reuse that informer; the rest open a metadata-only informer unless the predicate
	// needs the spec.
	newObject func() client.Object
	// anyWrite fires on every resourceVersion change. Only for kinds without status or
	// generation, where every write is a spec or data write.
	anyWrite bool
	// specChanged catches spec edits on kinds whose generation stays put and whose
	// status churns, so neither generation nor resourceVersion tells drift apart.
	specChanged func(oldObj, newObj client.Object) bool
	// ignoreUpdate drops updates that move generation without being drift.
	ignoreUpdate func(r *TenantReconciler, oldObj, newObj client.Object) bool
	// rollout admits the status transitions a Pending tenant config waits for.
	rollout func(r *TenantReconciler) predicate.Funcs
	// scope narrows the informer through cache.ByObject. ByObject applies to every
	// informer of the GVK, so only for kinds no other code reads or watches through
	// the cache.
	scope operandCacheScope
}

type operandCacheScope int

const (
	scopeCluster operandCacheScope = iota
	// scopePlatformNamespaces limits the informer to the app and gateway namespaces.
	scopePlatformNamespaces
	// scopeTracked limits a cluster-scoped informer to objects with tenant tracking labels.
	scopeTracked
)

// tenantOperands lists every kind rendered by the maas-api overlays and PostRender,
// except NetworkPolicy, which SetupWithManager already watches.
func tenantOperands() []tenantOperand {
	return []tenantOperand{
		// Typed informers LifecycleReconciler (Deployment, ConfigMap, ClusterRoleBinding)
		// and the model, subscription and auth-policy controllers (HTTPRoute) always hold.
		{
			gvk:          tenantreconcile.GVKDeployment,
			newObject:    func() client.Object { return &appsv1.Deployment{} },
			ignoreUpdate: (*TenantReconciler).scaledByHPA,
			rollout:      (*TenantReconciler).maasAPIRolloutChanged,
		},
		{gvk: tenantreconcile.GVKConfigMap, newObject: func() client.Object { return &corev1.ConfigMap{} }, anyWrite: true},
		{gvk: tenantreconcile.GVKClusterRoleBinding, newObject: func() client.Object { return &rbacv1.ClusterRoleBinding{} }, anyWrite: true},
		{gvk: tenantreconcile.GVKHTTPRoute, newObject: func() client.Object { return &gwapiv1.HTTPRoute{} }},
		// Unstructured informer shared with MaaSSubscriptionReconciler, which needs TRLPs
		// in model namespaces, so it stays cluster-wide.
		{gvk: tenantreconcile.GVKTokenRateLimitPolicy, crd: "tokenratelimitpolicies.kuadrant.io", newObject: unstructuredOf(tenantreconcile.GVKTokenRateLimitPolicy)},
		// The ExternalModel reconciler reads typed Services in model namespaces, so the
		// Service GVK cannot be scoped; metadata keeps the cluster-wide informer small.
		{gvk: tenantreconcile.GVKService, newObject: metadataOnly(tenantreconcile.GVKService), anyWrite: true},

		{gvk: tenantreconcile.GVKServiceAccount, newObject: metadataOnly(tenantreconcile.GVKServiceAccount), anyWrite: true, scope: scopePlatformNamespaces},
		{gvk: gvkClusterRole, newObject: metadataOnly(gvkClusterRole), anyWrite: true, scope: scopeTracked},
		{gvk: tenantreconcile.GVKCronJob, newObject: metadataOnly(tenantreconcile.GVKCronJob), scope: scopePlatformNamespaces},
		// HPA spec edits leave generation at 0 and its status churns with every metrics
		// sample, so the spec itself is compared.
		{
			gvk:         tenantreconcile.GVKHPA,
			newObject:   func() client.Object { return &autov2.HorizontalPodAutoscaler{} },
			specChanged: hpaSpecChanged,
			scope:       scopePlatformNamespaces,
		},
		{gvk: tenantreconcile.GVKTelemetryPolicy, crd: "telemetrypolicies.extensions.kuadrant.io", newObject: metadataOnly(tenantreconcile.GVKTelemetryPolicy), scope: scopePlatformNamespaces},
		{gvk: tenantreconcile.GVKDestinationRule, crd: "destinationrules.networking.istio.io", newObject: metadataOnly(tenantreconcile.GVKDestinationRule), scope: scopePlatformNamespaces},
		{gvk: tenantreconcile.GVKEnvoyFilter, crd: envoyFilterCRD, newObject: metadataOnly(tenantreconcile.GVKEnvoyFilter), scope: scopePlatformNamespaces},
		{gvk: tenantreconcile.GVKIstioTelemetry, crd: "telemetries.telemetry.istio.io", newObject: metadataOnly(tenantreconcile.GVKIstioTelemetry), scope: scopePlatformNamespaces},
		{gvk: gvkServiceMonitor, crd: "servicemonitors.monitoring.coreos.com", newObject: metadataOnly(gvkServiceMonitor), scope: scopePlatformNamespaces},
		{gvk: tenantreconcile.GVKCertificate, crd: "certificates.cert-manager.io", newObject: metadataOnly(tenantreconcile.GVKCertificate), scope: scopePlatformNamespaces},
	}
}

// TenantOperandCacheByObject narrows the informers that exist only for tenant operand
// watches; every other reader of these kinds bypasses the cache (unstructured reads).
// A future cached reader would see a ClusterRole without tracking labels as NotFound,
// and a namespaced object outside the app and gateway namespaces as an "unknown
// namespace for the cache" error, which IgnoreNotFound does not swallow.
//
// CRD-backed kinds are included only when the CRD serves the watched version now:
// cache options for an unknown GVK fail manager creation. A CRD installed later gets a
// cluster-wide informer until the next restart.
func TenantOperandCacheByObject(ctx context.Context, reader client.Reader, appNamespace, gatewayNamespace string) map[client.Object]cache.ByObject {
	out := map[client.Object]cache.ByObject{}
	for _, op := range tenantOperands() {
		var byObject cache.ByObject
		switch op.scope {
		case scopeCluster:
			continue
		case scopePlatformNamespaces:
			byObject.Namespaces = map[string]cache.Config{}
			for _, ns := range []string{appNamespace, gatewayNamespace} {
				if ns != "" {
					byObject.Namespaces[ns] = cache.Config{}
				}
			}
		case scopeTracked:
			byObject.Label = trackedSelector
		}
		if op.crd != "" && !crdServesVersion(lookupCRD(ctx, reader, op.crd), op.gvk.Version) {
			continue
		}
		out[op.newObject()] = byObject
	}
	return out
}

func metadataOnly(gvk schema.GroupVersionKind) func() client.Object {
	return func() client.Object {
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(gvk)
		return obj
	}
}

func unstructuredOf(gvk schema.GroupVersionKind) func() client.Object {
	return func() client.Object {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		return obj
	}
}

// setupTenantPlatformWatches registers the watches that replace polling in the tenant
// reconcile: operand drift, maas-api rollout, Gateway, Kuadrant wasm detection,
// dependency CRDs, prerequisites and the deletion wait on MaaS CR finalizers.
func (r *TenantReconciler) setupTenantPlatformWatches(ctx context.Context, c controller.Controller, mgr ctrl.Manager) error {
	kind := func(obj client.Object, h handler.EventHandler, preds ...predicate.Predicate) func() source.Source {
		return func() source.Source {
			return source.Kind(mgr.GetCache(), obj, h, preds...)
		}
	}

	operandHandler := handler.EnqueueRequestsFromMapFunc(r.mapTenantOperandToMaasTenantConfig)
	for _, op := range tenantOperands() {
		pred := predicate.Predicate(r.tenantOperandChanged(op))
		if op.rollout != nil {
			pred = predicate.Or(pred, op.rollout(r))
		}
		if err := watchWhenServed(ctx, c, mgr, op.crd, op.gvk.Version, kind(op.newObject(), operandHandler, pred)); err != nil {
			return err
		}
	}

	dependencyCRDs := make([]string, 0, len(tenantreconcile.Dependencies))
	for _, d := range tenantreconcile.Dependencies {
		dependencyCRDs = append(dependencyCRDs, d.CRD)
	}
	allTenants := handler.EnqueueRequestsFromMapFunc(r.enqueueAllTenants)
	watches := []struct {
		crd, version string
		source       func() source.Source
	}{
		{source: kind(&gwapiv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.mapGatewayToMaasTenantConfigs),
			createOrDeleteOnly(), predicate.NewPredicateFuncs(r.inGatewayNamespace))},
		// Kuadrant's per-gateway filter switches payload processing between Kuadrant- and
		// router-anchored ext_proc. Kuadrant rewrites its spec on every policy change on the
		// gateway, while RunPlatform only checks that it exists, so updates are not admitted.
		{
			crd: envoyFilterCRD, version: tenantreconcile.GVKEnvoyFilter.Version,
			source: kind(metadataOnly(tenantreconcile.GVKEnvoyFilter)(), handler.EnqueueRequestsFromMapFunc(r.mapKuadrantGatewayFilterToMaasTenantConfigs),
				createOrDeleteOnly(), predicate.NewPredicateFuncs(r.isKuadrantGatewayFilter)),
		},
		{source: kind(&extv1.CustomResourceDefinition{}, allTenants, crdNamed(dependencyCRDs...))},
		{
			crd: "authorinos.operator.authorino.kuadrant.io", version: tenantreconcile.GVKAuthorino.Version,
			source: kind(metadataOnly(tenantreconcile.GVKAuthorino)(), allTenants, predicate.GenerationChangedPredicate{}),
		},
		{source: kind(&maasv1alpha1.MaaSSubscription{}, handler.EnqueueRequestsFromMapFunc(r.mapDeletedTenantChildToMaasTenantConfig), deleteOnly())},
		{source: kind(&maasv1alpha1.MaaSAuthPolicy{}, handler.EnqueueRequestsFromMapFunc(r.mapDeletedTenantChildToMaasTenantConfig), deleteOnly())},
		{
			crd: "authpolicies.kuadrant.io", version: tenantreconcile.GVKAuthPolicy.Version,
			source: kind(unstructuredOf(tenantreconcile.GVKAuthPolicy)(), handler.EnqueueRequestsFromMapFunc(r.mapGatewayAuthPolicyToMaasTenantConfigs),
				deleteOnly(), predicate.NewPredicateFuncs(r.isGatewayAuthPolicy)),
		},
	}
	for _, w := range watches {
		if err := watchWhenServed(ctx, c, mgr, w.crd, w.version, w.source); err != nil {
			return err
		}
	}
	return nil
}

// watchWhenServed starts a watch now for built-in kinds and served CRDs, and defers it
// until the CRD serves the version otherwise: a static watch on an unserved kind fails
// the controller's initial cache sync and stops the manager.
func watchWhenServed(ctx context.Context, c controller.Controller, mgr ctrl.Manager, crd, version string, makeSource func() source.Source) error {
	if crd == "" || crdServesVersion(lookupCRD(ctx, mgr.GetAPIReader(), crd), version) {
		return c.Watch(makeSource())
	}
	return registerWatchWhenCRDServes(c, mgr, crd, version, makeSource)
}

// isTenantOperand matches objects ApplyRendered stamped with tenant tracking labels,
// either cluster-scoped or in a namespace the pipeline writes to.
func (r *TenantReconciler) isTenantOperand(obj client.Object) bool {
	if ns := obj.GetNamespace(); ns != "" && !r.isTenantPlatformNamespace(ns) {
		return false
	}
	return hasTenantTrackingLabels(obj.GetLabels())
}

func hasTenantTrackingLabels(l map[string]string) bool {
	return l[tenantreconcile.LabelTenantName] != "" || l[tenantreconcile.LabelTenantNamespace] != ""
}

func (r *TenantReconciler) mapTenantOperandToMaasTenantConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if !hasTenantTrackingLabels(l) {
		// Only the maas-api rollout predicate admits unlabelled objects: a maas-api
		// Deployment owned by another controller, which ApplyRendered skips. Those predate
		// per-tenant names (ODH's ModelsAsService component deployed "maas-api"), so only
		// the default tenant's name maps.
		if _, ok := obj.(*appsv1.Deployment); ok && obj.GetNamespace() == r.AppNamespace &&
			obj.GetName() == tenantreconcile.DefaultMaaSAPIDeploymentName {
			return r.enqueueDefaultTenant(ctx, obj)
		}
		return nil
	}
	if r.TenantNamespaceDiscoveryEnabled {
		if ns := l[tenantreconcile.LabelTenantNamespace]; ns != "" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{
				Name:      maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace: ns,
			}}}
		}
	}
	return r.enqueueDefaultTenant(ctx, obj)
}

// tenantOperandChanged admits operand creates and deletes, and updates that may be
// drift against the rendered object.
func (r *TenantReconciler) tenantOperandChanged(op tenantOperand) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.isTenantOperand(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return r.isTenantOperand(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !r.isTenantOperand(e.ObjectOld) && !r.isTenantOperand(e.ObjectNew) {
				return false
			}
			if op.ignoreUpdate != nil && op.ignoreUpdate(r, e.ObjectOld, e.ObjectNew) {
				return false
			}
			return tenantOperandDrifted(e.ObjectOld, e.ObjectNew, op)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// tenantOperandDrifted ignores updates that move the tracking labels from one tenant to
// another. Operands with a fixed name in the shared app namespace, and the cluster-scoped
// RBAC, are applied by every tenant and each apply stamps that tenant's labels.
// Re-enqueueing on the relabel would bounce between tenants indefinitely, whether or not
// the rest of the rendered object differs.
//
// Kinds that are not anyWrite never fire on resourceVersion alone, because their status
// is written by other controllers. A no-op server-side apply leaves resourceVersion
// unchanged, so the controller's own steady-state applies never fire either way.
func tenantOperandDrifted(oldObj, newObj client.Object, op tenantOperand) bool {
	if tenantRelabelled(oldObj.GetLabels(), newObj.GetLabels()) {
		return false
	}
	if !isManaged(oldObj) && !isManaged(newObj) {
		// Apply skips opendatahub.io/managed=false objects, so this cannot loop, but
		// readiness checks such as PayloadProcessingEnvoyFilterReady still read their spec.
		return oldObj.GetGeneration() != newObj.GetGeneration()
	}
	if oldObj.GetGeneration() != newObj.GetGeneration() {
		return true
	}
	if !maps.Equal(oldObj.GetLabels(), newObj.GetLabels()) || !maps.Equal(oldObj.GetAnnotations(), newObj.GetAnnotations()) {
		return true
	}
	if !equality.Semantic.DeepEqual(oldObj.GetOwnerReferences(), newObj.GetOwnerReferences()) {
		return true
	}
	if op.specChanged != nil && op.specChanged(oldObj, newObj) {
		return true
	}
	return op.anyWrite && oldObj.GetResourceVersion() != newObj.GetResourceVersion()
}

func tenantRelabelled(oldLabels, newLabels map[string]string) bool {
	for _, key := range []string{tenantreconcile.LabelTenantName, tenantreconcile.LabelTenantNamespace} {
		if oldLabels[key] != "" && newLabels[key] != "" && oldLabels[key] != newLabels[key] {
			return true
		}
	}
	return false
}

func hpaSpecChanged(oldObj, newObj client.Object) bool {
	oldHPA, okOld := oldObj.(*autov2.HorizontalPodAutoscaler)
	newHPA, okNew := newObj.(*autov2.HorizontalPodAutoscaler)
	return okOld && okNew && !equality.Semantic.DeepEqual(oldHPA.Spec, newHPA.Spec)
}

// scaledByHPA reports whether a Deployment update only moved spec.replicas while an HPA
// targets the Deployment. PostRender drops replicas from autoscaled Deployments, so the
// HPA's scaling is not drift.
func (r *TenantReconciler) scaledByHPA(oldObj, newObj client.Object) bool {
	oldDep, okOld := oldObj.(*appsv1.Deployment)
	newDep, okNew := newObj.(*appsv1.Deployment)
	if !okOld || !okNew || equality.Semantic.DeepEqual(oldDep.Spec.Replicas, newDep.Spec.Replicas) {
		return false
	}
	oldSpec := oldDep.Spec.DeepCopy()
	oldSpec.Replicas = newDep.Spec.Replicas
	if !equality.Semantic.DeepEqual(*oldSpec, newDep.Spec) ||
		!maps.Equal(oldDep.Labels, newDep.Labels) || !maps.Equal(oldDep.Annotations, newDep.Annotations) {
		return false
	}
	var hpas autov2.HorizontalPodAutoscalerList
	if err := r.List(context.Background(), &hpas, client.InNamespace(newDep.Namespace)); err != nil {
		return false
	}
	return slices.ContainsFunc(hpas.Items, func(h autov2.HorizontalPodAutoscaler) bool {
		return h.Spec.ScaleTargetRef.Kind == "Deployment" && h.Spec.ScaleTargetRef.Name == newDep.Name
	})
}

func (r *TenantReconciler) isMaaSAPIDeployment(obj client.Object) bool {
	return obj.GetNamespace() == r.AppNamespace && tenantreconcile.IsMaaSAPIDeploymentName(obj.GetName())
}

// maasAPIRolloutChanged fires when a maas-api Deployment crosses the readiness gate in
// tenantreconcile.MaasAPIDeploymentReady, which is what a Pending tenant waits for.
func (r *TenantReconciler) maasAPIRolloutChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.isMaaSAPIDeployment(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return r.isMaaSAPIDeployment(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !r.isMaaSAPIDeployment(e.ObjectNew) {
				return false
			}
			oldDep, okOld := e.ObjectOld.(*appsv1.Deployment)
			newDep, okNew := e.ObjectNew.(*appsv1.Deployment)
			if !okOld || !okNew {
				return false
			}
			oldReady, _ := tenantreconcile.DeploymentRolledOut(oldDep)
			newReady, _ := tenantreconcile.DeploymentRolledOut(newDep)
			return oldReady != newReady
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// inGatewayNamespace matches the only namespace a tenant gateway ref can point at.
// The reconcile reads a Gateway only for existence, so create and delete are enough.
func (r *TenantReconciler) inGatewayNamespace(obj client.Object) bool {
	return obj.GetNamespace() == r.GatewayNamespace
}

func (r *TenantReconciler) mapGatewayToMaasTenantConfigs(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.tenantsUsingGateway(ctx, obj.GetNamespace(), obj.GetName())
}

func (r *TenantReconciler) isKuadrantGatewayFilter(obj client.Object) bool {
	_, ok := tenantreconcile.KuadrantGatewayForResource(obj.GetName())
	return ok && r.inGatewayNamespace(obj)
}

func (r *TenantReconciler) mapKuadrantGatewayFilterToMaasTenantConfigs(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := tenantreconcile.KuadrantGatewayForResource(obj.GetName())
	if !ok {
		return nil
	}
	return r.tenantsUsingGateway(ctx, obj.GetNamespace(), gateway)
}

// tenantsUsingGateway resolves each reconciled tenant config's gateway the same way
// validateConfigAndGateway does. Tenants whose platform context does not resolve are
// skipped: they wait on their AITenant, not on a Gateway.
func (r *TenantReconciler) tenantsUsingGateway(ctx context.Context, namespace, name string) []reconcile.Request {
	var list maasv1alpha1.MaasTenantConfigList
	if err := r.List(ctx, &list); err != nil {
		oteljson.FromContext(ctx).Error(err, "failed to list MaasTenantConfigs for Gateway mapping")
		return nil
	}
	fallback := fallbackTenantGatewayRef(r.GatewayName, r.GatewayNamespace)
	var requests []reconcile.Request
	for i := range list.Items {
		tenant := &list.Items[i]
		if !r.reconcilesTenantConfig(tenant) {
			continue
		}
		platformContext, err := tenantreconcile.ResolvePlatformContext(ctx, r.Client, tenant, fallback)
		if err != nil {
			continue
		}
		if platformContext.GatewayRef.Namespace == namespace && platformContext.GatewayRef.Name == name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
		}
	}
	return requests
}

// reconcilesTenantConfig mirrors the namespace and name gate at the top of reconcile.
func (r *TenantReconciler) reconcilesTenantConfig(tenant *maasv1alpha1.MaasTenantConfig) bool {
	if r.TenantNamespaceDiscoveryEnabled {
		return true
	}
	return tenant.Name == maasv1alpha1.MaasTenantConfigInstanceName &&
		(r.TenantNamespace == "" || tenant.Namespace == r.TenantNamespace)
}

// isGatewayAuthPolicy matches the per-gateway AuthPolicy that ensureGatewayManagementAuth
// bootstraps when missing.
func (r *TenantReconciler) isGatewayAuthPolicy(obj client.Object) bool {
	return r.inGatewayNamespace(obj) && obj.GetLabels()["app.kubernetes.io/part-of"] == gatewayAuthPolicyPartOf
}

func (r *TenantReconciler) mapGatewayAuthPolicyToMaasTenantConfigs(ctx context.Context, obj client.Object) []reconcile.Request {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	gatewayName, _, _ := unstructured.NestedString(u.Object, "spec", "targetRef", "name")
	if gatewayName == "" {
		return nil
	}
	return r.tenantsUsingGateway(ctx, u.GetNamespace(), gatewayName)
}

// mapDeletedTenantChildToMaasTenantConfig re-enqueues a deleting tenant config once a
// MaaSSubscription or MaaSAuthPolicy in its namespace is gone, which is what
// handleDeletion waits for before removing the cleanup finalizer.
func (r *TenantReconciler) mapDeletedTenantChildToMaasTenantConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	key := types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: obj.GetNamespace()}
	var tenant maasv1alpha1.MaasTenantConfig
	if err := r.Get(ctx, key, &tenant); err != nil {
		if !apierrors.IsNotFound(err) {
			oteljson.FromContext(ctx).Error(err, "failed to get MaasTenantConfig for deleted MaaS CR mapping", "tenantConfig", key)
		}
		return nil
	}
	if tenant.DeletionTimestamp.IsZero() {
		return nil
	}
	return []reconcile.Request{{NamespacedName: key}}
}

func crdNamed(names ...string) predicate.Funcs {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return slices.Contains(names, o.GetName())
	})
}

func deleteOnly() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func createOrDeleteOnly() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
