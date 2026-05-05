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
	"strings"
	"time"

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

const (
	tenantFinalizer = "maas.opendatahub.io/tenant-finalizer"
)

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

	// Handle delete before Removed/Unmanaged idle so we still run teardown when the CR is being deleted.
	if !tenant.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&tenant, tenantFinalizer) {
			return ctrl.Result{}, nil
		}
		pending, err := r.finalizeTenantDeletion(ctx, &tenant)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: finalizeRequeueInterval}, nil
		}
		patchBase := client.MergeFrom(tenant.DeepCopy())
		controllerutil.RemoveFinalizer(&tenant, tenantFinalizer)
		if err := r.Patch(ctx, &tenant, patchBase); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	ms := managementState(tenant.Annotations)
	if ms == managementStateRemoved || ms == managementStateUnmanaged {
		return r.handleIdleManagementState(ctx, &tenant, ms)
	}

	if !controllerutil.ContainsFinalizer(&tenant, tenantFinalizer) {
		patchBase := client.MergeFrom(tenant.DeepCopy())
		controllerutil.AddFinalizer(&tenant, tenantFinalizer)
		if err := r.Patch(ctx, &tenant, patchBase); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if ms != "" && ms != managementStateManaged {
		if err := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "UnexpectedManagementState",
			fmt.Sprintf("unsupported %s=%q", managementStateAnnotation, ms)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

	runRes, err := tenantreconcile.RunPlatform(ctx, log, r.Client, r.Scheme, &tenant, r.ManifestPath, appNs)
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

// handleIdleManagementState handles Removed and Unmanaged states.
// Removed tears down owned resources before dropping the finalizer;
// Unmanaged simply drops the finalizer, leaving resources in place.
func (r *TenantReconciler) handleIdleManagementState(ctx context.Context, tenant *maasv1alpha1.Tenant, ms string) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, tenant, "", metav1.ConditionFalse, "ManagementStateIdle",
		fmt.Sprintf("management state is %q; platform workloads are not driven by this reconciler in this state", ms)); err != nil {
		return ctrl.Result{}, err
	}
	if controllerutil.ContainsFinalizer(tenant, tenantFinalizer) {
		if ms == managementStateRemoved {
			pending, err := r.finalizeTenantDeletion(ctx, tenant)
			if err != nil {
				return ctrl.Result{}, err
			}
			if pending {
				return ctrl.Result{RequeueAfter: finalizeRequeueInterval}, nil
			}
		}
		patchBase := client.MergeFrom(tenant.DeepCopy())
		controllerutil.RemoveFinalizer(tenant, tenantFinalizer)
		if err := r.Patch(ctx, tenant, patchBase); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
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
