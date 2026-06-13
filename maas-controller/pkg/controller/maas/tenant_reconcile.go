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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// Handle delete before Unmanaged idle. Config anchor lifecycle is owned by the operator /
	// ModelsAsService GC and the lifecycle reconciler; the Tenant reconciler does not delete Config.
	if !tenant.DeletionTimestamp.IsZero() {
		// Clean up gateway AuthPolicy before allowing deletion
		if err := r.cleanupGatewayAuthPolicy(ctx, log, &tenant); err != nil {
			log.Error(err, "failed to cleanup gateway AuthPolicy")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	ms := managementState(tenant.Annotations)
	if ms == managementStateUnmanaged {
		return r.handleIdleManagementState(ctx, &tenant, ms)
	}

	if ms != "" && ms != managementStateManaged && ms != managementStateRemoved {
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

	// Removed: operator-driven teardown deletes the Config anchor; readyConfigOrWait already
	// surfaces ConfigMissing / ConfigTerminating. If Config is still live, suspend platform apply
	// until GC removes it (do not treat like Unmanaged — no platform while anchor exists).
	if managementState(tenant.Annotations) == managementStateRemoved {
		log.V(1).Info("Tenant in Removed management state with live Config; waiting for anchor teardown")
		if err := r.patchStatus(ctx, &tenant, "Pending", metav1.ConditionFalse, "WaitingForRemovedTeardown",
			"management state is Removed; platform reconcile is suspended until the Config anchor is deleted by component GC"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	orig := tenant.DeepCopy()
	if err := r.applyGatewayDefaults(&tenant); err != nil {
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

	appNs := r.appNamespaceForTenant()
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

	runRes, err := tenantreconcile.RunPlatform(ctx, log, r.Client, r.Scheme, &tenant, r.ManifestPath, appNs, r.ClusterAudience, mcfg)
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

	// Reconcile gateway AuthPolicy after successful platform deployment
	// Extract tenant identifier for correct tenant isolation
	tenantID, err := tenantreconcile.TenantIdentifierFor(&tenant)
	if err != nil {
		log.Error(err, "failed to determine tenant identifier")
		if err2 := r.patchStatus(ctx, &tenant, "Failed", metav1.ConditionFalse, "TenantIDResolutionFailed", err.Error()); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// TEMPORARY: Skip gateway AuthPolicy - let MaaSAuthPolicyReconciler handle it to debug rate limiting
	_ = tenantID // avoid unused var error
	if false {
		if err := r.reconcileGatewayAuthPolicy(ctx, &tenant, tenantID, appNs); err != nil {
			log.Error(err, "failed to reconcile gateway AuthPolicy", "tenantID", tenantID)
			setDeploymentsAvailableCondition(&tenant, false, "AuthPolicyReconcileFailed", err.Error())
			if err2 := r.patchStatus(ctx, &tenant, "Degraded", metav1.ConditionFalse, "AuthPolicyReconcileFailed", err.Error()); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	// Clean up legacy maas-api deployment from opendatahub/redhat-ods-applications namespace
	// after successful deployment to the infrastructure namespace. Use sync.Once + boolean flag
	// to retry on transient failures while avoiding redundant GET requests after success.
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
	// All maas-api instances deploy to the operator namespace (opendatahub for ODH,
	// redhat-ods-applications for RHOAI). The shared database secret also lives in this namespace.
	return tenantreconcile.DefaultMaaSAPINamespace
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
	// Clean up maas-api resources from legacy namespaces.
	// Currently no legacy namespaces - maas-api deploys to operator namespace
	// (opendatahub for ODH, redhat-ods-applications for RHOAI).
	legacyNamespaces := []string{}

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
