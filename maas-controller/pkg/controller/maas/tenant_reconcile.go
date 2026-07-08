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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	tenantFinalizer = "maas.opendatahub.io/tenant-cleanup"
)

// tenantUsesCleanupFinalizer reports whether this Tenant should carry tenant-cleanup.
// The default platform tenant (no AITenant labels) relies on Config/default GC for teardown (TODO: fix in GA release);
// only AITenant-managed tenants need explicit per-tenant resource cleanup on delete.
func tenantUsesCleanupFinalizer(tenant *maasv1alpha1.Tenant) (bool, error) {
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return false, err
	}
	return tenantreconcile.TenantUsesAITenantPlatformContext(tenant) || tenantID != "", nil
}

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

	// When tenant namespace discovery is disabled, only reconcile the default tenant
	// in the configured TenantNamespace. When enabled, reconcile all Tenant CRs cluster-wide.
	if !r.TenantNamespaceDiscoveryEnabled {
		if r.TenantNamespace != "" && tenant.Namespace != r.TenantNamespace {
			log.V(1).Info("ignoring Tenant outside configured platform tenant namespace",
				"tenantNamespace", tenant.Namespace,
				"configuredTenantNamespace", r.TenantNamespace)
			return ctrl.Result{}, nil
		}

		if tenant.Name != maasv1alpha1.TenantInstanceName {
			return ctrl.Result{}, nil
		}
	}

	// Guard against unlabeled Tenant CRs in foreign namespaces when discovery is enabled.
	// Without LabelManagedByAITenant, TenantIdentifierFor returns "" (default tenant),
	// which would cause the rendered maas-api Deployment to use the base name "maas-api"
	// and SSA-overwrite the actual default tenant's Deployment with wrong env vars
	// (e.g., MAAS_SUBSCRIPTION_NAMESPACE pointing at the foreign namespace).
	if r.TenantNamespaceDiscoveryEnabled && r.TenantNamespace != "" && tenant.Namespace != r.TenantNamespace {
		labels := tenant.GetLabels()
		if labels == nil || labels[tenantreconcile.LabelManagedByAITenant] != "true" {
			log.V(1).Info("ignoring unlabeled Tenant in foreign namespace to prevent default-tenant resource collision",
				"tenantNamespace", tenant.Namespace,
				"defaultTenantNamespace", r.TenantNamespace,
				"hint", "set maas.opendatahub.io/managed-by-aitenant=true and maas.opendatahub.io/tenant-name labels")
			return ctrl.Result{}, nil
		}
	}

	usesCleanupFinalizer, err := tenantUsesCleanupFinalizer(&tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !tenant.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, log, &tenant)
	}

	if usesCleanupFinalizer {
		if !controllerutil.ContainsFinalizer(&tenant, tenantFinalizer) {
			controllerutil.AddFinalizer(&tenant, tenantFinalizer)
			if err := r.Update(ctx, &tenant); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if controllerutil.ContainsFinalizer(&tenant, tenantFinalizer) {
		// Converge upgraded clusters: default-tenant teardown is owned by Config GC.
		controllerutil.RemoveFinalizer(&tenant, tenantFinalizer)
		if err := r.Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle management states
	if result, err := r.handleManagementState(ctx, log, &tenant); result != nil {
		return *result, err
	}

	// Validate config and gateway
	mcfg, platformContext, result, err := r.validateConfigAndGateway(ctx, log, &tenant, req)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check dependencies and prerequisites
	if result, err := r.checkDependenciesAndPrerequisites(ctx, &tenant); result != nil {
		return *result, err
	}

	// Run platform reconciliation
	result, err = r.reconcilePlatform(ctx, log, &tenant, platformContext, mcfg)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Cleanup legacy resources
	r.attemptLegacyCleanup(ctx, log)

	// Set final status
	return r.setFinalStatus(ctx, &tenant)
}

func (r *TenantReconciler) handleDeletion(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(tenant, tenantFinalizer) {
		if err := r.cleanupMaaSSubscriptions(ctx, log, tenant); err != nil {
			log.Error(err, "failed to cleanup MaaSSubscriptions")
			return ctrl.Result{}, err
		}

		if err := r.cleanupMaaSAuthPolicies(ctx, log, tenant); err != nil {
			log.Error(err, "failed to cleanup MaaSAuthPolicies")
			return ctrl.Result{}, err
		}

		if err := r.cleanupTenantResources(ctx, log, tenant); err != nil {
			log.Error(err, "failed to cleanup tenant resources")
			return ctrl.Result{}, err
		}

		controllerutil.RemoveFinalizer(tenant, tenantFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) handleManagementState(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) (*ctrl.Result, error) {
	ms := managementState(tenant.Annotations)
	if ms == managementStateUnmanaged {
		res, err := r.handleIdleManagementState(ctx, tenant, ms)
		return &res, err
	}

	if ms != "" && ms != managementStateManaged && ms != managementStateRemoved {
		if err := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "UnexpectedManagementState",
			fmt.Sprintf("unsupported %s=%q", managementStateAnnotation, ms)); err != nil {
			return nil, err
		}
		res := ctrl.Result{RequeueAfter: 30 * time.Second}
		return &res, nil
	}

	return nil, nil
}

func (r *TenantReconciler) validateConfigAndGateway(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant, req ctrl.Request) (*maasv1alpha1.Config, tenantreconcile.PlatformContext, *ctrl.Result, error) {
	mcfg, wait, err := r.readyConfigOrWait(ctx, log, tenant)
	if err != nil {
		return nil, tenantreconcile.PlatformContext{}, nil, err
	}
	if wait != nil {
		return nil, tenantreconcile.PlatformContext{}, wait, nil
	}

	if managementState(tenant.Annotations) == managementStateRemoved {
		log.V(1).Info("Tenant in Removed management state with live Config; waiting for anchor teardown")
		if err := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "WaitingForRemovedTeardown",
			"management state is Removed; platform reconcile is suspended until the Config anchor is deleted by component GC"); err != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err
		}
		res := ctrl.Result{RequeueAfter: 10 * time.Second}
		return nil, tenantreconcile.PlatformContext{}, &res, nil
	}

	fallbackGatewayRef := fallbackTenantGatewayRef(r.GatewayName, r.GatewayNamespace)
	platformContext, err := tenantreconcile.ResolvePlatformContext(ctx, r.Client, tenant, fallbackGatewayRef)
	if err != nil {
		if err2 := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "InvalidGateway", err.Error()); err2 != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err2
		}
		res := ctrl.Result{RequeueAfter: 30 * time.Second}
		return nil, tenantreconcile.PlatformContext{}, &res, nil
	}

	if !tenantreconcile.TenantUsesAITenantPlatformContext(tenant) {
		orig := tenant.DeepCopy()
		if err := r.applyGatewayDefaults(tenant); err != nil {
			if err2 := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "InvalidGateway", err.Error()); err2 != nil {
				return nil, tenantreconcile.PlatformContext{}, nil, err2
			}
			res := ctrl.Result{RequeueAfter: 30 * time.Second}
			return nil, tenantreconcile.PlatformContext{}, &res, nil
		}
		if orig.Spec.GatewayRef != tenant.Spec.GatewayRef {
			if err := r.Patch(ctx, tenant, client.MergeFrom(orig)); err != nil {
				return nil, tenantreconcile.PlatformContext{}, nil, err
			}
			if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
				return nil, tenantreconcile.PlatformContext{}, nil, err
			}
		}
	}

	if err := validateGatewayExists(ctx, r.Client, platformContext.GatewayRef.Namespace, platformContext.GatewayRef.Name); err != nil {
		log.Info("gateway validation failed", "error", err)
		if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "GatewayNotReady", err.Error()); err2 != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err2
		}
		res := ctrl.Result{RequeueAfter: 30 * time.Second}
		return nil, tenantreconcile.PlatformContext{}, &res, nil
	}

	if r.ManifestPath == "" {
		if err := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "ManifestPathUnset",
			"MAAS_PLATFORM_MANIFESTS is not set and no default kustomize path resolved; cannot apply platform manifests"); err != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err
		}
		res := ctrl.Result{RequeueAfter: 2 * time.Minute}
		return nil, tenantreconcile.PlatformContext{}, &res, nil
	}

	return mcfg, platformContext, nil, nil
}

func (r *TenantReconciler) checkDependenciesAndPrerequisites(ctx context.Context, tenant *maasv1alpha1.Tenant) (*ctrl.Result, error) {
	if err := tenantreconcile.CheckDependencies(ctx, r.Client); err != nil {
		setDependenciesCondition(tenant, false, err.Error())
		setDeploymentsAvailableCondition(tenant, false, "DependenciesNotMet", err.Error())
		prerequisitesUnevaluatedCondition(tenant, "Prerequisites were not evaluated because required dependencies are not met")
		if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "DependenciesNotAvailable", err.Error()); err2 != nil {
			return nil, err2
		}
		res := ctrl.Result{RequeueAfter: 45 * time.Second}
		return &res, nil
	}
	setDependenciesCondition(tenant, true, "")

	appNs := r.appNamespaceForTenant()
	rep := tenantreconcile.CollectPrerequisiteReport(ctx, r.Client, appNs)
	setPrerequisiteConditionsFromReport(tenant, rep)
	if len(rep.Blocking) > 0 {
		tenant.Status.Phase = "Failed"
		agg := fmt.Sprintf("%s; %s", strings.Join(rep.Blocking, "; "), strings.Join(rep.Warnings, "; "))
		setDeploymentsAvailableCondition(tenant, false, "PrerequisitesMissing", agg)
		apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               tenantreconcile.ReadyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "PrerequisitesNotMet",
			Message:            agg,
			ObservedGeneration: tenant.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, tenant); err != nil {
			return nil, err
		}
		res := ctrl.Result{RequeueAfter: 45 * time.Second}
		return &res, nil
	}

	return nil, nil
}

func (r *TenantReconciler) reconcilePlatform(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant, platformContext tenantreconcile.PlatformContext, mcfg *maasv1alpha1.Config) (*ctrl.Result, error) {
	appNs := r.appNamespaceForTenant()
	runRes, err := tenantreconcile.RunPlatform(ctx, log, r.Client, r.Scheme, tenant, platformContext, r.ManifestPath, appNs, r.ClusterAudience, mcfg)
	if err != nil {
		log.Error(err, "Tenant platform reconcile failed")
		setDeploymentsAvailableCondition(tenant, false, "PlatformReconcileFailed", err.Error())
		if err2 := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "PlatformReconcileFailed", err.Error()); err2 != nil {
			return nil, err2
		}
		res := ctrl.Result{RequeueAfter: 45 * time.Second}
		return &res, nil
	}

	if runRes.DeploymentPending {
		tenant.Status.Phase = "Pending"
		setDeploymentsAvailableCondition(tenant, false, "DeploymentsNotReady", runRes.Detail)
		apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
			Type:               tenantreconcile.ReadyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentsNotReady",
			Message:            runRes.Detail,
			ObservedGeneration: tenant.Generation,
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, tenant); err != nil {
			return nil, err
		}
		res := ctrl.Result{RequeueAfter: 20 * time.Second}
		return &res, nil
	}

	return nil, nil
}

func (r *TenantReconciler) attemptLegacyCleanup(ctx context.Context, log logr.Logger) {
	r.cleanupMu.Lock()
	if !r.cleanupCompleted {
		r.cleanupMu.Unlock()
		r.cleanupOnce.Do(func() {
			if err := r.cleanupLegacyMaaSAPIDeployment(ctx, log); err != nil {
				log.V(1).Info("failed to clean up legacy maas-api deployment (will retry)", "error", err)
				return
			}
			r.cleanupMu.Lock()
			r.cleanupCompleted = true
			r.cleanupMu.Unlock()
		})
	} else {
		r.cleanupMu.Unlock()
	}
}

func (r *TenantReconciler) setFinalStatus(ctx context.Context, tenant *maasv1alpha1.Tenant) (ctrl.Result, error) {
	tenant.Status.Phase = "Active"
	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		tenant.Status.Phase = "Degraded"
	}
	setDeploymentsAvailableCondition(tenant, true, "DeploymentsReady", "maas-api deployment is available")
	apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               tenantreconcile.ReadyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "MaaS platform manifests applied and maas-api deployment is available",
		ObservedGeneration: tenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

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

// handleIdleManagementState handles Unmanaged: platform workloads are not driven by this
// reconciler; record idle status.
func (r *TenantReconciler) handleIdleManagementState(ctx context.Context, tenant *maasv1alpha1.Tenant, ms string) (ctrl.Result, error) {
	if err := r.patchStatus(ctx, tenant, "", metav1.ConditionFalse, "ManagementStateIdle",
		fmt.Sprintf("management state is %q; platform workloads are not driven by this reconciler in this state", ms)); err != nil {
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

func (r *TenantReconciler) appNamespaceForTenant() string {
	return r.AppNamespace
}

func (r *TenantReconciler) applyGatewayDefaults(tenant *maasv1alpha1.Tenant) error {
	ref := &tenant.Spec.GatewayRef
	if ref.Namespace == "" && ref.Name == "" {
		ref.Namespace = r.GatewayNamespace
		ref.Name = r.GatewayName
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

func (r *TenantReconciler) cleanupLegacyMaaSAPIDeployment(ctx context.Context, log logr.Logger) error {
	// Clean up maas-api resources from legacy namespaces during namespace separation migration.
	// When infrastructure namespace differs from controller namespace (separation is enabled),
	// automatically clean up old maas-api deployment from the controller namespace.
	legacyNamespaces := []string{}

	if r.ControllerNamespace != "" && r.AppNamespace != r.ControllerNamespace {
		// Separation is enabled - clean up old deployment from controller namespace
		legacyNamespaces = []string{r.ControllerNamespace}
		log.Info("Infrastructure namespace separation detected, will clean up legacy maas-api",
			"infraNamespace", r.AppNamespace,
			"controllerNamespace", r.ControllerNamespace,
			"legacyNamespaces", legacyNamespaces)
	}

	for _, ns := range legacyNamespaces {
		// Check if legacy Deployment exists
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: "maas-api"}, &dep)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("check for legacy maas-api deployment in %s: %w", ns, err)
		}

		if err == nil {
			// Found legacy deployment - verify it's ours before deleting
			labels := dep.GetLabels()
			if labels == nil || labels["app.kubernetes.io/part-of"] != "models-as-a-service" {
				log.Info("Skipping deletion of maas-api deployment - not owned by MaaS", "namespace", ns)
				continue
			}

			// Found legacy deployment - clean up all related resources
			log.Info("Cleaning up legacy maas-api resources", "namespace", ns)

			// Delete Deployment
			if err := r.Delete(ctx, &dep); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete legacy maas-api deployment from %s: %w", ns, err)
			}

			// Delete Service
			if err := r.Delete(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "maas-api", Namespace: ns},
			}); err != nil && !apierrors.IsNotFound(err) {
				log.V(1).Info("failed to delete legacy Service (non-fatal)", "namespace", ns, "error", err)
			}

			// Delete HTTPRoute
			if err := r.Delete(ctx, &gwapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "maas-api-route", Namespace: ns},
			}); err != nil && !apierrors.IsNotFound(err) {
				log.V(1).Info("failed to delete legacy HTTPRoute (non-fatal)", "namespace", ns, "error", err)
			}

			// Delete ConfigMap (if any)
			if err := r.Delete(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "maas-api-config", Namespace: ns},
			}); err != nil && !apierrors.IsNotFound(err) {
				log.V(1).Info("failed to delete legacy ConfigMap (non-fatal)", "namespace", ns, "error", err)
			}

			log.Info("Successfully cleaned up legacy maas-api resources", "namespace", ns)
		}
	}

	return nil
}

// cleanupTenantResources deletes per-tenant maas-api resources when the Tenant is being deleted.
// These resources are owned by Config/default (for lifecycle management), not by the Tenant,
// so they won't be garbage collected automatically and must be explicitly deleted.
func (r *TenantReconciler) cleanupTenantResources(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) error {
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return err
	}

	tenantName, err := tenantreconcile.TenantNameFor(tenant)
	if err != nil {
		log.Error(err, "failed to resolve tenant name for cleanup logging, continuing with fallback")
		tenantName = tenantID
		if tenantName == "" {
			tenantName = tenant.Namespace
		}
	}

	appNs := r.appNamespaceForTenant()
	gatewayNs := r.GatewayNamespace
	log.Info("Cleaning up per-tenant maas-api resources", "tenant", tenantName, "tenantID", tenantID, "appNamespace", appNs, "gatewayNamespace", gatewayNs)

	appResourcesToDelete := []struct {
		gvk       schema.GroupVersionKind
		name      string
		namespace string
	}{
		{
			gvk:       tenantreconcile.GVKDeployment,
			name:      tenantreconcile.MaaSAPIDeploymentName(tenantID),
			namespace: appNs,
		},
		{
			gvk:       tenantreconcile.GVKService,
			name:      tenantreconcile.MaaSAPIServiceName(tenantID),
			namespace: appNs,
		},
		{
			gvk:       tenantreconcile.GVKHTTPRoute,
			name:      tenantreconcile.MaaSAPIRouteName(tenantID),
			namespace: appNs,
		},
		{
			gvk:       tenantreconcile.GVKCronJob,
			name:      tenantreconcile.MaaSAPIKeyCleanupCronJobName(tenantID),
			namespace: appNs,
		},
		{
			gvk:       tenantreconcile.GVKAuthPolicy,
			name:      tenantreconcile.MaaSAPIAuthPolicyName(tenantID),
			namespace: appNs,
		},
	}

	gatewayResourcesToDelete := []struct {
		gvk       schema.GroupVersionKind
		name      string
		namespace string
	}{
		{
			gvk:       tenantreconcile.GVKTokenRateLimitPolicy,
			name:      tenantreconcile.GatewayTokenRateLimitDefaultDenyPolicyName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKDestinationRule,
			name:      tenantreconcile.GatewayDestinationRuleName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKTelemetryPolicy,
			name:      tenantreconcile.TelemetryPolicyName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKIstioTelemetry,
			name:      tenantreconcile.IstioTelemetryName(tenantID),
			namespace: gatewayNs,
		},
	}

	for _, res := range append(appResourcesToDelete, gatewayResourcesToDelete...) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(res.gvk)
		obj.SetName(res.name)
		obj.SetNamespace(res.namespace)

		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete tenant resource",
				"gvk", res.gvk.String(),
				"name", res.name,
				"namespace", res.namespace)
			return err
		}
		log.Info("Deleted tenant resource", "gvk", res.gvk.Kind, "name", res.name, "namespace", res.namespace)
	}

	return nil
}

// cleanupMaaSSubscriptions deletes all MaaSSubscription CRs in the tenant namespace.
// MaaSSubscription finalizers clean up generated TokenRateLimitPolicies.
func (r *TenantReconciler) cleanupMaaSSubscriptions(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) error {
	log.Info("Cleaning up MaaSSubscription CRs", "namespace", tenant.Namespace)

	subscriptionList := &maasv1alpha1.MaaSSubscriptionList{}
	if err := r.List(ctx, subscriptionList, client.InNamespace(tenant.Namespace)); err != nil {
		return fmt.Errorf("failed to list MaaSSubscriptions: %w", err)
	}

	for i := range subscriptionList.Items {
		subscription := &subscriptionList.Items[i]
		log.Info("Deleting MaaSSubscription", "name", subscription.Name, "namespace", subscription.Namespace)
		if err := r.Delete(ctx, subscription); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete MaaSSubscription %s/%s: %w", subscription.Namespace, subscription.Name, err)
		}
	}

	log.Info("Cleaned up MaaSSubscription CRs", "count", len(subscriptionList.Items))
	return nil
}

// cleanupMaaSAuthPolicies deletes all MaaSAuthPolicy CRs in the tenant namespace.
// MaaSAuthPolicyReconciler's handleDeletion will clean up the gateway AuthPolicy
// when the last MaaSAuthPolicy is deleted.
func (r *TenantReconciler) cleanupMaaSAuthPolicies(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant) error {
	log.Info("Cleaning up MaaSAuthPolicy CRs", "namespace", tenant.Namespace)

	// List all MaaSAuthPolicy CRs in this namespace
	policyList := &maasv1alpha1.MaaSAuthPolicyList{}
	if err := r.List(ctx, policyList, client.InNamespace(tenant.Namespace)); err != nil {
		return fmt.Errorf("failed to list MaaSAuthPolicies: %w", err)
	}

	// Delete each MaaSAuthPolicy
	for i := range policyList.Items {
		policy := &policyList.Items[i]
		log.Info("Deleting MaaSAuthPolicy", "name", policy.Name, "namespace", policy.Namespace)
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete MaaSAuthPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}

	log.Info("Cleaned up MaaSAuthPolicy CRs", "count", len(policyList.Items))
	return nil
}
