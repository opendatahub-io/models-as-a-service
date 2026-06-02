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
		// NOTE: Owner references from cluster-scoped Config to namespace-scoped Deployment/Tenant
		// are not allowed by Kubernetes GC. Lifecycle management is handled through controller
		// watches instead (TenantReconciler watches Config, and Config deletion triggers cleanup).
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

// NOTE: The ensureDeploymentReferencesConfig and ensureTenantReferencesConfig functions have been
// removed because Kubernetes does not allow owner references from cluster-scoped resources (Config)
// to namespace-scoped resources (Deployment, Tenant). Such owner references are rejected by the
// admission controller and cause immediate garbage collection of the dependent resource.
//
// Lifecycle management is instead handled through:
// 1. TenantReconciler watches Config and suspends platform reconcile when Config is missing/terminating
// 2. Operator-driven teardown (ODH) deletes Config, which triggers TenantReconciler to stop work
// 3. Operator then deletes Deployment and Tenant CRs directly (no automatic GC cascade)

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
