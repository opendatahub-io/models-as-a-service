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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// Annotations mirrored from ODH (avoid importing opendatahub-operator).
const (
	managementStateAnnotation = "component.opendatahub.io/management-state"
	managementStateManaged    = "Managed"
	managementStateRemoved    = "Removed"
	managementStateUnmanaged  = "Unmanaged"
)

// legacyTenantFinalizer was used before teardown moved to Config GC + management-state Removed.
// It is stripped on delete / idle reconcile, and proactively on any non-deleting reconcile so
// upgraded clusters lose the finalizer without a delete or annotation change.
const legacyTenantFinalizer = "maas.opendatahub.io/tenant-finalizer"

func managementState(ann map[string]string) string {
	if ann == nil {
		return ""
	}
	return ann[managementStateAnnotation]
}

func (r *TenantReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var tenant maasv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if tenant.Name != maasv1alpha1.TenantInstanceName {
		return ctrl.Result{}, nil
	}

	// Handle delete before Removed/Unmanaged idle. Legacy installs may still carry the old finalizer.
	// When Config exists, delete it first (platform GC); otherwise only strip the legacy finalizer.
	if !tenant.DeletionTimestamp.IsZero() {
		var cfg maasv1alpha1.Config
		switch err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); {
		case err == nil:
			requeue, err2 := r.ensureConfigDeletedForRemoval(ctx)
			if err2 != nil {
				return ctrl.Result{}, err2
			}
			if requeue != nil {
				return *requeue, nil
			}
		case apierrors.IsNotFound(err):
			// No anchor: proceed to finalizer strip only.
		default:
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.stripLegacyTenantFinalizerIfPresent(ctx, &tenant)
	}

	// Proactively strip the legacy finalizer on non-deleting reconciles (startup / steady state)
	// so clusters upgraded from older releases converge without manual patch.
	if tenant.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(&tenant, legacyTenantFinalizer) {
		if err := r.stripLegacyTenantFinalizerIfPresent(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}
	}

	ms := managementState(tenant.Annotations)
	if ms == managementStateRemoved || ms == managementStateUnmanaged {
		return r.handleIdleManagementState(ctx, &tenant, ms)
	}

	if ms != "" && ms != managementStateManaged {
		if err := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "UnexpectedManagementState",
			fmt.Sprintf("unsupported %s=%q", managementStateAnnotation, ms)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	mcfg, wait, err := r.readyConfigOrWait(ctx, log, &tenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != nil {
		return *wait, nil
	}

	orig := tenant.DeepCopy()
	if err := applyGatewayDefaults(&tenant); err != nil {
		if err2 := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "InvalidGateway", err.Error()); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if orig.Spec.GatewayRef != tenant.Spec.GatewayRef {
		if err := r.Patch(ctx, &tenant, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := validateGatewayExists(ctx, r.Client, tenant.Spec.GatewayRef.Namespace, tenant.Spec.GatewayRef.Name); err != nil {
		log.Info("gateway validation failed", "error", err)
		if err2 := r.patchStatus(ctx, &tenant, "Pending", metav1.ConditionFalse, "GatewayNotReady", err.Error()); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if r.ManifestPath == "" {
		if err := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "ManifestPathUnset",
			"MAAS_PLATFORM_MANIFESTS is not set and no default kustomize path resolved; cannot apply platform manifests"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	}

	if err := tenantreconcile.CheckDependencies(ctx, r.Client); err != nil {
		log.Info("Tenant dependency check failed", "error", err)
		setDependenciesCondition(&tenant, false, err.Error())
		setDeploymentsAvailableCondition(&tenant, false, "DependenciesNotMet", err.Error())
		prerequisitesUnevaluatedCondition(&tenant, "Prerequisites were not evaluated because required dependencies are not met")
		if err2 := r.patchStatus(ctx, &tenant, "Pending", metav1.ConditionFalse, "DependenciesNotAvailable", err.Error()); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}
	setDependenciesCondition(&tenant, true, "")

	appNs := r.AppNamespace
	rep := tenantreconcile.CollectPrerequisiteReport(ctx, r.Client, appNs)
	setPrerequisiteConditionsFromReport(&tenant, rep)
	if len(rep.Blocking) > 0 {
		tenant.Status.Phase = "Failed"
		agg := strings.Join(append(append([]string{}, rep.Blocking...), rep.Warnings...), "; ")
		setDeploymentsAvailableCondition(&tenant, false, "PrerequisitesMissing", agg)
		apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               tenantreconcile.ReadyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "PrerequisitesNotMet",
			Message:            agg,
			ObservedGeneration: tenant.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	runRes, err := tenantreconcile.RunPlatform(ctx, log, r.Client, r.Scheme, &tenant, r.ManifestPath, appNs, mcfg)
	if err != nil {
		log.Error(err, "Tenant platform reconcile failed")
		setDeploymentsAvailableCondition(&tenant, false, "PlatformReconcileFailed", err.Error())
		if err2 := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "PlatformReconcileFailed", err.Error()); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	if runRes.DeploymentPending {
		tenant.Status.Phase = "Pending"
		setDeploymentsAvailableCondition(&tenant, false, "DeploymentsNotReady", runRes.Detail)
		apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               tenantreconcile.ReadyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentsNotReady",
			Message:            runRes.Detail,
			ObservedGeneration: tenant.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	tenant.Status.Phase = "Active"
	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		tenant.Status.Phase = "Degraded"
	}
	setDeploymentsAvailableCondition(&tenant, true, "DeploymentsReady", "maas-api deployment is available")
	apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               tenantreconcile.ReadyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "MaaS platform manifests applied and maas-api deployment is available",
		ObservedGeneration: tenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, &tenant); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("Tenant platform reconciled", "name", tenant.Name)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// readyConfigOrWait returns the singleton Config when it exists, is not deleting,
// and has a UID. Otherwise it updates Tenant status and returns a Result the caller should return
// immediately without running gateway, dependency, prerequisite, or platform work.
func (r *TenantReconciler) readyConfigOrWait(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) (*maasv1alpha1.Config, *ctrl.Result, error) {
	var ct maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &ct); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Config not found; skipping reconcile until it exists", "name", maasv1alpha1.ConfigInstanceName)
			if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "ConfigMissing",
				fmt.Sprintf("Config %q is required before platform apply", maasv1alpha1.ConfigInstanceName)); err2 != nil {
				return nil, nil, err2
			}
			res := ctrl.Result{RequeueAfter: 10 * time.Second}
			return nil, &res, nil
		}
		return nil, nil, err
	}
	if !ct.DeletionTimestamp.IsZero() {
		log.Info("Config is terminating; skipping platform reconcile", "name", ct.Name)
		if err := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "ConfigTerminating",
			fmt.Sprintf("Config %q is deleting; platform reconcile is suspended until the anchor is gone or recreated", ct.Name)); err != nil {
			return nil, nil, err
		}
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return nil, &res, nil
	}
	if ct.UID == "" {
		if err := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "WaitingForConfigUID",
			fmt.Sprintf("Config %q has no UID yet; waiting before platform apply", maasv1alpha1.ConfigInstanceName)); err != nil {
			return nil, nil, err
		}
		res := ctrl.Result{RequeueAfter: 5 * time.Second}
		return nil, &res, nil
	}
	return &ct, nil, nil
}

// SkipConfigBootstrap returns true when the default Tenant is in Removed management state.
// The manager startup runnable must not recreate Config or patch owner refs in that state,
// because Removed reconciliation deletes Config to drive platform GC.
func SkipConfigBootstrap(tenant *maasv1alpha1.Tenant) bool {
	if tenant == nil || tenant.Annotations == nil {
		return false
	}
	return tenant.Annotations[managementStateAnnotation] == managementStateRemoved
}

func (r *TenantReconciler) stripLegacyTenantFinalizerIfPresent(ctx context.Context, tenant *maasv1alpha1.Tenant) error {
	if !controllerutil.ContainsFinalizer(tenant, legacyTenantFinalizer) {
		return nil
	}
	base := client.MergeFrom(tenant.DeepCopy())
	controllerutil.RemoveFinalizer(tenant, legacyTenantFinalizer)
	return r.Patch(ctx, tenant, base)
}

// ensureConfigDeletedForRemoval issues delete on Config/default when present.
// It requeues while the object is still terminating so callers can observe progress.
func (r *TenantReconciler) ensureConfigDeletedForRemoval(ctx context.Context) (*ctrl.Result, error) {
	var ct maasv1alpha1.Config
	key := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	if err := r.Get(ctx, key, &ct); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !ct.DeletionTimestamp.IsZero() {
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return &res, nil
	}
	if err := r.Delete(ctx, &ct); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	res := ctrl.Result{RequeueAfter: 10 * time.Second}
	return &res, nil
}

// handleIdleManagementState handles Removed and Unmanaged states.
// Removed deletes Config/default so platform operands are garbage-collected; Unmanaged
// leaves resources in place. Any legacy Tenant finalizer from older releases is stripped.
func (r *TenantReconciler) handleIdleManagementState(ctx context.Context, tenant *maasv1alpha1.Tenant, ms string) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, tenant, "", metav1.ConditionFalse, "ManagementStateIdle",
		fmt.Sprintf("management state is %q; platform workloads are not driven by this reconciler in this state", ms)); err != nil {
		return ctrl.Result{}, err
	}
	if ms == managementStateRemoved {
		requeue, err := r.ensureConfigDeletedForRemoval(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.stripLegacyTenantFinalizerIfPresent(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
		if requeue != nil {
			return *requeue, nil
		}
		return ctrl.Result{}, nil
	}
	if err := r.stripLegacyTenantFinalizerIfPresent(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) operatorNamespace() string {
	if r.OperatorNamespace != "" {
		return r.OperatorNamespace
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return os.Getenv("WATCH_NAMESPACE")
}

func applyGatewayDefaults(tenant *maasv1alpha1.Tenant) error {
	ref := &tenant.Spec.GatewayRef
	if ref.Namespace == "" && ref.Name == "" {
		ref.Namespace = tenantreconcile.DefaultGatewayNamespace
		ref.Name = tenantreconcile.DefaultGatewayName
		return nil
	}
	if ref.Namespace == "" || ref.Name == "" {
		return errors.New("invalid gateway specification: when specifying a custom gateway, both namespace and name must be provided")
	}
	return nil
}

func validateGatewayExists(ctx context.Context, c client.Client, namespace, name string) error {
	gw := &gwapiv1.Gateway{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := c.Get(ctx, key, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("gateway %s/%s not found: the specified Gateway must exist before enabling MaaS platform reconcile", namespace, name)
		}
		return fmt.Errorf("failed to look up gateway %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (r *TenantReconciler) patchStatus(ctx context.Context, tenant *maasv1alpha1.Tenant, phase string, status metav1.ConditionStatus, reason, message string) error {
	tenant.Status.Phase = phase
	apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               tenantreconcile.ReadyConditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
	return r.Status().Update(ctx, tenant)
}
