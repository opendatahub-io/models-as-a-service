/*
Copyright 2025.

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
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// CleanupFinalizer was historically added to the maas-controller Deployment for coordinated
// teardown when ODH removed MaaS. It is no longer set; this constant remains so reconciles
// can strip it from older installs.
const CleanupFinalizer = "maas.opendatahub.io/cleanup"

// LifecycleReconciler watches the maas-controller Deployment. It is the sole creator of the
// cluster-scoped Config/default anchor when the Deployment exists and is not terminating (so
// standalone installs do not race applying a Config manifest before the Config CRD is ready).
// It links the Deployment, default AITenant, and default MaasTenantConfig to Config via non-controller
// ownerReferences (same relationship shape for all). Legacy CleanupFinalizer entries are removed when present.
type LifecycleReconciler struct {
	client.Client
	Scheme                      *runtime.Scheme
	DeploymentName              string
	DeploymentNS                string
	TenantSubscriptionNamespace string
	AITenantNamespace           string
	ObservabilityManifestsPath  string
	MonitoringNamespace         string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=configs,verbs=get;list;watch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=maastenantconfigs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=aitenants,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=perses.dev,resources=persesdashboards;persesdatasources,verbs=get;list;watch;create;patch;delete

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("self-deployment").WithValues("deployment", req.NamespacedName)

	var dep appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if dep.DeletionTimestamp.IsZero() {
		if res, err := r.ensureSingletonConfig(ctx, &dep); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
		}
		if res, err := r.ensureDeploymentReferencesConfig(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
		}
		if res, err := r.ensureDefaultAITenantReferencesConfig(ctx); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
		}
		if res, err := r.ensureTenantReferencesConfig(ctx); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
		}
		if err := r.ensureObservability(ctx, log); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.stripLegacyCleanupFinalizer(ctx, log, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Terminating: remove legacy finalizer only so deletion is not blocked.
	if err := r.stripLegacyCleanupFinalizer(ctx, log, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensureDefaultAITenantReferencesConfig links the automatically bootstrapped
// default AITenant to Config/default. The bootstrap runnable may create the
// AITenant shell before owner refs converge.
func (r *LifecycleReconciler) ensureDefaultAITenantReferencesConfig(ctx context.Context) (*ctrl.Result, error) {
	if r.AITenantNamespace == "" {
		return nil, nil
	}
	if r.Scheme == nil {
		return nil, nil
	}
	log := ctrl.LoggerFrom(ctx)
	cfgKey := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, cfgKey, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Config anchor not found when linking default AITenant; requeueing")
			res := ctrl.Result{RequeueAfter: 2 * time.Second}
			return &res, nil
		}
		return nil, err
	}
	if !cfg.DeletionTimestamp.IsZero() {
		log.Info("Config anchor is terminating when linking default AITenant; requeueing")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, nil
	}
	if cfg.UID == "" {
		res := ctrl.Result{RequeueAfter: 2 * time.Second}
		return &res, nil
	}

	aitenantKey := client.ObjectKey{Name: tenantreconcile.DefaultAITenantName, Namespace: r.AITenantNamespace}
	var aitenant maasv1alpha1.AITenant
	if err := r.Get(ctx, aitenantKey, &aitenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if aitenantReferencesConfig(&aitenant, &cfg) {
		return nil, nil
	}
	base := aitenant.DeepCopy()
	if err := controllerutil.SetOwnerReference(&cfg, &aitenant, r.Scheme); err != nil {
		return nil, fmt.Errorf("set Config owner reference on default AITenant: %w", err)
	}
	if err := r.Patch(ctx, &aitenant, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("patch default AITenant ownerReferences: %w", err)
	}
	log.Info("set Config owner reference on default AITenant", "namespace", r.AITenantNamespace)
	return nil, nil
}

func (r *LifecycleReconciler) stripLegacyCleanupFinalizer(ctx context.Context, log logr.Logger, key types.NamespacedName) error {
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(&dep, CleanupFinalizer) {
		return nil
	}
	base := dep.DeepCopy()
	controllerutil.RemoveFinalizer(&dep, CleanupFinalizer)
	if err := r.Patch(ctx, &dep, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("remove legacy cleanup finalizer from Deployment: %w", err)
	}
	log.Info("removed legacy cleanup finalizer from Deployment")
	return nil
}

// ensureSingletonConfig creates Config/default when it is missing and the watched Deployment
// is still running. If Config is terminating, requeues until teardown completes (avoids racing
// intentional anchor deletion). After accidental deletion while the Deployment remains, the
// anchor is recreated on a later reconcile.
func (r *LifecycleReconciler) ensureSingletonConfig(ctx context.Context, dep *appsv1.Deployment) (*ctrl.Result, error) {
	if dep == nil || !dep.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	key := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	var cfg maasv1alpha1.Config
	switch err := r.Get(ctx, key, &cfg); {
	case err == nil:
		if !cfg.DeletionTimestamp.IsZero() {
			return &ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if cfg.UID == "" {
			return &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return nil, nil
	case apierrors.IsNotFound(err):
		toCreate := &maasv1alpha1.Config{
			TypeMeta: metav1.TypeMeta{
				APIVersion: maasv1alpha1.GroupVersion.String(),
				Kind:       maasv1alpha1.ConfigKind,
			},
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName},
		}
		if err := r.Create(ctx, toCreate); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := r.Get(ctx, key, &cfg); err != nil {
			if apierrors.IsNotFound(err) {
				return &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return nil, err
		}
		if cfg.UID == "" {
			return &ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return nil, nil
	default:
		return nil, err
	}
}

// ensureDeploymentReferencesConfig links the controller Deployment to Config/default
// via a non-controller ownerReference so the workload participates in the same GC graph as other
// operands without competing with the ODH operator's controller owner (when present).
//
// Call after ensureSingletonConfig in the same reconcile: Config should exist with a UID and
// not be terminating. If this function still observes a missing, terminating, or UID-less anchor
// (cache lag or races), it logs and returns a short requeue instead of succeeding with no work.
func (r *LifecycleReconciler) ensureDeploymentReferencesConfig(ctx context.Context, key types.NamespacedName) (*ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("deployment", key)
	if r.Scheme == nil {
		return nil, nil
	}
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Config anchor not found when linking Deployment; requeueing (singleton reconcile should create it)")
			res := ctrl.Result{RequeueAfter: 2 * time.Second}
			return &res, nil
		}
		return nil, err
	}
	if !cfg.DeletionTimestamp.IsZero() {
		log.Info("Config anchor is terminating when linking Deployment; requeueing until teardown completes")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, nil
	}
	if cfg.UID == "" {
		log.Info("Config anchor has no UID yet when linking Deployment; requeueing")
		res := ctrl.Result{RequeueAfter: 2 * time.Second}
		return &res, nil
	}
	var dep appsv1.Deployment
	if err := r.Get(ctx, key, &dep); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	for _, ref := range dep.OwnerReferences {
		if ref.UID == cfg.UID && ref.Kind == maasv1alpha1.ConfigKind && ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return nil, nil
		}
	}
	base := dep.DeepCopy()
	if err := controllerutil.SetOwnerReference(&cfg, &dep, r.Scheme); err != nil {
		return nil, fmt.Errorf("set Config owner reference on deployment: %w", err)
	}
	if err := r.Patch(ctx, &dep, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("patch deployment ownerReferences: %w", err)
	}
	log.Info("set Config owner reference on maas-controller Deployment")
	return nil, nil
}

// ensureTenantReferencesConfig links MaasTenantConfig/default-tenant to Config/default via the same non-controller
// ownerReference pattern as the Deployment. The cluster bootstrap runnable may create the config
// shell without owner refs; this reconciler converges them once Config has a UID.
func (r *LifecycleReconciler) ensureTenantReferencesConfig(ctx context.Context) (*ctrl.Result, error) {
	if r.TenantSubscriptionNamespace == "" {
		return nil, nil
	}
	if r.Scheme == nil {
		return nil, nil
	}
	log := ctrl.LoggerFrom(ctx)
	cfgKey := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, cfgKey, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Config anchor not found when linking MaasTenantConfig; requeueing")
			res := ctrl.Result{RequeueAfter: 2 * time.Second}
			return &res, nil
		}
		return nil, err
	}
	if !cfg.DeletionTimestamp.IsZero() {
		log.Info("Config anchor is terminating when linking MaasTenantConfig; requeueing")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, nil
	}
	if cfg.UID == "" {
		res := ctrl.Result{RequeueAfter: 2 * time.Second}
		return &res, nil
	}
	tKey := client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: r.TenantSubscriptionNamespace}
	var tenant maasv1alpha1.MaasTenantConfig
	if err := r.Get(ctx, tKey, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if tenantReferencesConfig(&tenant, &cfg) {
		return nil, nil
	}
	base := tenant.DeepCopy()
	if err := controllerutil.SetOwnerReference(&cfg, &tenant, r.Scheme); err != nil {
		return nil, fmt.Errorf("set Config owner reference on MaasTenantConfig: %w", err)
	}
	if err := r.Patch(ctx, &tenant, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("patch MaasTenantConfig ownerReferences: %w", err)
	}
	log.Info("set Config owner reference on MaasTenantConfig/default-tenant", "namespace", r.TenantSubscriptionNamespace)
	return nil, nil
}

func tenantReferencesConfig(tenant *maasv1alpha1.MaasTenantConfig, ct *maasv1alpha1.Config) bool {
	for _, ref := range tenant.OwnerReferences {
		if ref.UID == ct.UID &&
			ref.Kind == maasv1alpha1.ConfigKind &&
			ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return true
		}
	}
	return false
}

func aitenantReferencesConfig(aitenant *maasv1alpha1.AITenant, ct *maasv1alpha1.Config) bool {
	for _, ref := range aitenant.OwnerReferences {
		if ref.UID == ct.UID &&
			ref.Kind == maasv1alpha1.ConfigKind &&
			ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return true
		}
	}
	return false
}

func (r *LifecycleReconciler) ensureObservability(ctx context.Context, log logr.Logger) error {
	if err := r.ensureLimitadorServiceMonitor(ctx); err != nil {
		return err
	}
	if err := r.ensureUsageDashboard(ctx, log); err != nil {
		return err
	}
	return nil
}

// ensureLimitadorServiceMonitor creates or updates the Limitador ServiceMonitor in the operator namespace.
// This ServiceMonitor ensures metrics are scraped from the Limitador pod and get to the DSC's monitoring stack.
// If the ServiceMonitor CRD is not available, this is a no-op (allows running without the monitoring stack).
// TODO: need to set the overall status of MaaS to Degraded if COO is missing.
func (r *LifecycleReconciler) ensureLimitadorServiceMonitor(ctx context.Context) error {
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	scrapeInterval := cfg.Spec.LimitadorScrapeInterval
	if scrapeInterval == "" {
		scrapeInterval = "30s"
	}

	sm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1",
			"kind":       "ServiceMonitor",
			"metadata": map[string]any{
				"name":      "limitador-metrics",
				"namespace": r.MonitoringNamespace,
				"labels": map[string]any{
					"app":                              "limitador",
					"monitoring.opendatahub.io/scrape": "true",
				},
			},
			"spec": map[string]any{
				"endpoints": []any{
					map[string]any{
						"interval": scrapeInterval,
						"path":     "/metrics",
						"port":     "http",
					},
				},
				"namespaceSelector": map[string]any{
					"any": true,
				},
				"selector": map[string]any{
					"matchLabels": map[string]any{
						"app": "limitador",
					},
				},
			},
		},
	}

	if err := controllerutil.SetOwnerReference(&cfg, sm, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on ServiceMonitor: %w", err)
	}

	if err := r.Patch(ctx, sm, client.Apply, client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
		// If ServiceMonitor CRD is not installed, skip creation (monitoring stack is optional)
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("apply ServiceMonitor: %w", err)
	}

	return nil
}

// ensureUsageDashboard creates the usage dashboard in the monitoring namespace.
// Uses the existing kustomize infrastructure to render manifests from ObservabilityManifestsPath.
// If ObservabilityManifestsPath is not set or Perses CRDs are not installed, gracefully skips.
func (r *LifecycleReconciler) ensureUsageDashboard(ctx context.Context, log logr.Logger) error {
	// Skip if observability manifests path not configured
	if r.ObservabilityManifestsPath == "" {
		log.Info("WARNING: Observability manifests path not configured; skipping observability dashboards")
		return nil
	}

	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// Render kustomization (reuses tenant reconciler's kustomize logic)
	// TODO: move kustomize logic to a separate package and reuse it here.
	resources, err := tenantreconcile.RenderKustomize(r.ObservabilityManifestsPath, r.MonitoringNamespace)
	if err != nil {
		return fmt.Errorf("render observability dashboards: %w", err)
	}

	// Apply each resource with Config as controller owner
	for _, resource := range resources {
		res := resource // avoid loop variable aliasing
		if err := controllerutil.SetControllerReference(&cfg, &res, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference on %s %s: %w", res.GetKind(), res.GetName(), err)
		}

		if err := r.Patch(ctx, &res, client.Apply, client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
			if isOptionalAPIGroup(res.GroupVersionKind().Group) && (apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err)) {
				// CRD not yet registered for a known optional dependency (e.g. Perses CRDs
				// installed by COO which may not be present yet). Skip so the rest of the
				// platform manifests are applied and Tenant reconcile does not fail.
				// The CRD watch will re-trigger reconcile once the CRDs appear.
				ctrl.LoggerFrom(ctx).Info("skipping resource: optional CRD not yet registered, will apply once installed",
					"group", res.GroupVersionKind().Group, "kind", res.GetKind(),
					"name", res.GetName(), "namespace", res.GetNamespace())
				continue
			}
			return fmt.Errorf("apply %s %s/%s: %w", res.GetKind(), res.GetNamespace(), res.GetName(), err)
		}
	}

	return nil
}

// SetupWithManager registers the controller to watch only the maas-controller Deployment.
func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	selfOnly := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == r.DeploymentName && o.GetNamespace() == r.DeploymentNS
	})
	cfgSingleton := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == maasv1alpha1.ConfigInstanceName
	})
	defaultTenant := predicate.NewPredicateFuncs(func(o client.Object) bool {
		if r.TenantSubscriptionNamespace == "" {
			return false
		}
		return o.GetNamespace() == r.TenantSubscriptionNamespace && o.GetName() == maasv1alpha1.MaasTenantConfigInstanceName
	})
	defaultAITenant := predicate.NewPredicateFuncs(func(o client.Object) bool {
		if r.AITenantNamespace == "" {
			return false
		}
		return o.GetNamespace() == r.AITenantNamespace && o.GetName() == tenantreconcile.DefaultAITenantName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(selfOnly)).
		Watches(
			&maasv1alpha1.Config{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(cfgSingleton),
		).
		Watches(
			&maasv1alpha1.MaasTenantConfig{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(defaultTenant),
		).
		Watches(
			&maasv1alpha1.AITenant{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(defaultAITenant),
		).
		// Re-reconcile when optional operator CRDs (e.g. Perses from COO) are installed
		// so that resources previously skipped due to missing CRDs are applied immediately.
		Watches(
			&extv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(crdInOptionalAPIGroup()),
		).
		Complete(r)
}

// crdInOptionalAPIGroup matches CRDs belonging to optional platform operator API groups
// (e.g. perses.dev from COO). CRD names follow the pattern "<plural>.<group>", so a
// suffix check is sufficient to identify the group without parsing the spec.
func crdInOptionalAPIGroup() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		for group := range OptionalAPIGroups {
			if strings.HasSuffix(o.GetName(), "."+group) {
				return true
			}
		}
		return false
	})
}
