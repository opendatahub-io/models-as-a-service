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
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// CleanupFinalizer was historically added to the maas-controller Deployment for coordinated
// teardown when ODH removed MaaS. It is no longer set; this constant remains so reconciles
// can strip it from older installs.
const CleanupFinalizer = "maas.opendatahub.io/cleanup"

// LifecycleReconciler watches the maas-controller Deployment. It is the sole creator of the
// cluster-scoped Config/default anchor when the Deployment exists and is not terminating (so
// standalone installs do not race applying a Config manifest before the Config CRD is ready).
// It links the Deployment and default Tenant to Config via non-controller ownerReferences (same
// relationship shape for both). Legacy CleanupFinalizer entries are removed when present.
type LifecycleReconciler struct {
	client.Client
	Scheme                      *runtime.Scheme
	DeploymentName              string
	DeploymentNS                string
	TenantSubscriptionNamespace string
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=configs,verbs=get;list;watch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch;update;patch

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
		if res, err := r.ensureTenantReferencesConfig(ctx); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
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

// ensureTenantReferencesConfig links default-tenant to Config/default via the same non-controller
// ownerReference pattern as the Deployment. The cluster bootstrap runnable may create the Tenant
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
			log.Info("Config anchor not found when linking Tenant; requeueing")
			res := ctrl.Result{RequeueAfter: 2 * time.Second}
			return &res, nil
		}
		return nil, err
	}
	if !cfg.DeletionTimestamp.IsZero() {
		log.Info("Config anchor is terminating when linking Tenant; requeueing")
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, nil
	}
	if cfg.UID == "" {
		res := ctrl.Result{RequeueAfter: 2 * time.Second}
		return &res, nil
	}
	tKey := client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: r.TenantSubscriptionNamespace}
	var tenant maasv1alpha1.Tenant
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
		return nil, fmt.Errorf("set Config owner reference on tenant: %w", err)
	}
	if err := r.Patch(ctx, &tenant, client.MergeFrom(base)); err != nil {
		return nil, fmt.Errorf("patch tenant ownerReferences: %w", err)
	}
	log.Info("set Config owner reference on default-tenant", "namespace", r.TenantSubscriptionNamespace)
	return nil, nil
}

func tenantReferencesConfig(tenant *maasv1alpha1.Tenant, ct *maasv1alpha1.Config) bool {
	for _, ref := range tenant.OwnerReferences {
		if ref.UID == ct.UID &&
			ref.Kind == maasv1alpha1.ConfigKind &&
			ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return true
		}
	}
	return false
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
		return o.GetNamespace() == r.TenantSubscriptionNamespace && o.GetName() == maasv1alpha1.TenantInstanceName
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
			&maasv1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: r.DeploymentNS,
					Name:      r.DeploymentName,
				}}}
			}),
			builder.WithPredicates(defaultTenant),
		).
		Complete(r)
}
