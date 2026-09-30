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
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// Annotations mirrored from ODH (avoid importing opendatahub-operator).
const (
	managementStateAnnotation = "component.opendatahub.io/management-state"
	managementStateManaged    = "Managed"
	managementStateRemoved    = "Removed"
	managementStateUnmanaged  = "Unmanaged"

	tenantFinalizer       = "maas.opendatahub.io/tenant-cleanup"
	legacyTenantFinalizer = "maas.opendatahub.io/tenant-finalizer"
)

const (
	// kuadrantRecheckInterval bounds how long router-anchored payload processing outlives
	// Kuadrant wiring its wasm filter. Until the next render, wasm runs before ipp-pre and
	// body-routed /v1/* skips AuthPolicy and TRLP, so the window is kept well under the old
	// 5m resync; the cost is one render and apply per affected tenant, only while degraded.
	kuadrantRecheckInterval = 2 * time.Minute

	// kuadrantWasmPluginRecheckInterval is how often the WasmPlugin probe re-detects the
	// Kuadrant carrier, and how often a tenant whose WasmPlugin could not be read is
	// re-rendered. It matches the 5m resync every tenant ran before watches replaced it.
	// The unreadable case keeps the Kuadrant anchors, so the auth window above does not apply.
	kuadrantWasmPluginRecheckInterval = 5 * time.Minute

	// discoveryLagRetry covers the gap between a CRD turning Established, which fires the
	// CRD watch, and discovery serving its kind to the REST mapper. No further event
	// arrives once that gap closes.
	discoveryLagRetry = 10 * time.Second
)

// kuadrantRouterFallbackWarning is static so the periodic recheck never rewrites status.
var kuadrantRouterFallbackWarning = "Kuadrant wasm auth was not found on the gateway; payload processing " +
	"runs on router anchors until it appears, re-detected every " + kuadrantRecheckInterval.String()

type timerReasonKey struct{}

// timerReason is why a reconcile asked to come back on a timer rather than wait for a
// watch event; Reconcile logs it, as nothing else shows a tenant waiting on a timer.
type timerReason struct{ reason string }

func withTimerReason(ctx context.Context) (context.Context, *timerReason) {
	reason := &timerReason{}
	return context.WithValue(ctx, timerReasonKey{}, reason), reason
}

func requeueOnTimer(ctx context.Context, after time.Duration, reason string) ctrl.Result {
	if holder, ok := ctx.Value(timerReasonKey{}).(*timerReason); ok {
		holder.reason = reason
	}
	return ctrl.Result{RequeueAfter: after}
}

// kuadrantRecheck requeues while the Kuadrant wasm detection cannot be left to a watch or
// to the WasmPlugin probe: on the router fallback, and while the WasmPlugin cannot be read.
func kuadrantRecheck(ctx context.Context, runRes *tenantreconcile.RunResult) ctrl.Result {
	switch {
	case runRes == nil:
		return ctrl.Result{}
	case runRes.KuadrantRouterFallback:
		return requeueOnTimer(ctx, kuadrantRecheckInterval, "Kuadrant wasm auth not found on the gateway; payload processing runs on the router fallback")
	case runRes.KuadrantDetectionWarning != "":
		return requeueOnTimer(ctx, kuadrantWasmPluginRecheckInterval, "Kuadrant wasm auth could not be verified: "+runRes.KuadrantDetectionWarning)
	}
	return ctrl.Result{}
}

// tenantUsesCleanupFinalizer reports whether this tenant config should carry tenant-cleanup.
// The default platform tenant (no AITenant labels) relies on Config/default GC for teardown (TODO: fix in GA release);
// only AITenant-managed tenants need explicit per-tenant resource cleanup on delete.
func tenantUsesCleanupFinalizer(tenant *maasv1alpha1.MaasTenantConfig) (bool, error) {
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
	log := oteljson.FromContext(ctx)

	var tenant maasv1alpha1.MaasTenantConfig
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// When tenant namespace discovery is disabled, only reconcile the default tenant config
	// in the configured TenantNamespace. When enabled, reconcile all MaasTenantConfig CRs cluster-wide.
	if !r.TenantNamespaceDiscoveryEnabled {
		if r.TenantNamespace != "" && tenant.Namespace != r.TenantNamespace {
			log.V(1).Info("ignoring MaasTenantConfig outside configured platform tenant namespace",
				"tenantNamespace", tenant.Namespace,
				"configuredTenantNamespace", r.TenantNamespace)
			return ctrl.Result{}, nil
		}

		if tenant.Name != maasv1alpha1.MaasTenantConfigInstanceName {
			return ctrl.Result{}, nil
		}
	}

	// Guard against unlabeled tenant configs in foreign namespaces when discovery is enabled.
	// Without LabelManagedByAITenant, TenantIdentifierFor returns "" (default tenant),
	// which would cause the rendered maas-api Deployment to use the base name "maas-api"
	// and SSA-overwrite the actual default tenant's Deployment with wrong env vars
	// (e.g., MAAS_SUBSCRIPTION_NAMESPACE pointing at the foreign namespace).
	if r.TenantNamespaceDiscoveryEnabled && r.TenantNamespace != "" && tenant.Namespace != r.TenantNamespace {
		labels := tenant.GetLabels()
		if labels == nil || labels[tenantreconcile.LabelManagedByAITenant] != "true" {
			log.V(1).Info("ignoring unlabeled MaasTenantConfig in foreign namespace to prevent default-tenant resource collision",
				"tenantNamespace", tenant.Namespace,
				"defaultTenantNamespace", r.TenantNamespace,
				"hint", "set maas.opendatahub.io/managed-by-aitenant=true and maas.opendatahub.io/tenant-name labels")
			return ctrl.Result{}, nil
		}
	}

	// Handle deletion before tenant identifier validation and the teardown guard:
	// finalizer cleanup must proceed even when TenantIdentifierFor would fail or while
	// LifecycleReconciler is tearing down MaaS (AITenant deletion waits on MaasTenantConfig).
	if !tenant.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, log, &tenant)
	}

	// Skip reconciliation during MaaS teardown to avoid blocking on gateway dependencies.
	// When teardown is requested, LifecycleReconciler orchestrates cleanup independently.
	// Try both controller namespace and app namespace (for deployments in operator infra namespace).
	for _, depNS := range []string{r.ControllerNamespace, r.AppNamespace} {
		if depNS == "" {
			continue
		}
		var dep appsv1.Deployment
		depKey := client.ObjectKey{Name: "maas-controller", Namespace: depNS}
		if err := r.Get(ctx, depKey, &dep); err == nil && TeardownRequestedOnDeployment(&dep) {
			log.Info("skipping MaasTenantConfig reconciliation during MaaS teardown", "deploymentNamespace", depNS)
			return ctrl.Result{}, nil
		}
	}

	usesCleanupFinalizer, err := tenantUsesCleanupFinalizer(&tenant)
	if err != nil {
		// Only a label fix helps, and that passes the MaasTenantConfig watch.
		return ctrl.Result{}, r.recordFailure(ctx, &tenant, "InvalidTenantIdentity", reconcile.TerminalError(err))
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

	// Surface the infrastructure namespace so operators know where maas-db-config lives.
	tenant.Status.InfraNamespace = r.appNamespaceForTenant()

	if err := r.deleteUsageLogsEnvoyFilterIfDisabled(ctx, log, &tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete usage-logs EnvoyFilter after usageLogging disabled: %w",
			r.recordFailure(ctx, &tenant, "UsageLogsCleanupFailed", err))
	}

	// Handle management states
	if result, err := r.handleManagementState(ctx, log, &tenant); result != nil {
		return *result, err
	}

	// Validate config and gateway
	mcfg, platformContext, result, err := r.validateConfigAndGateway(ctx, log, &tenant)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check dependencies and prerequisites
	prereqReport, result, err := r.checkDependenciesAndPrerequisites(ctx, &tenant)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Run platform reconciliation
	runRes, result, err := r.reconcilePlatform(ctx, log, &tenant, platformContext, mcfg)
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	usageLogsWarning, err := r.ensureUsageLogsEnvoyFilter(ctx, log, &tenant, platformContext, mcfg)
	if err != nil {
		return ctrl.Result{}, r.recordFailure(ctx, &tenant, "UsageLogsReconcileFailed", err)
	}

	// Aggregate all warnings and set Degraded condition once
	r.aggregateWarningsAndSetDegraded(&tenant, prereqReport, runRes, usageLogsWarning)

	// Cleanup legacy resources
	r.attemptLegacyCleanup(ctx, log)

	// Set final status
	return r.setFinalStatus(ctx, &tenant, runRes)
}

func (r *TenantReconciler) handleDeletion(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(tenant, tenantFinalizer) {
		subscriptionsDeleted, err := r.cleanupMaaSSubscriptions(ctx, log, tenant)
		if err != nil {
			log.Error(err, "failed to cleanup MaaSSubscriptions")
			return ctrl.Result{}, err
		}

		authPoliciesDeleted, err := r.cleanupMaaSAuthPolicies(ctx, log, tenant)
		if err != nil {
			log.Error(err, "failed to cleanup MaaSAuthPolicies")
			return ctrl.Result{}, err
		}
		if !subscriptionsDeleted || !authPoliciesDeleted {
			// Their delete events re-enqueue this tenant (see mapDeletedTenantChildToMaasTenantConfig).
			return ctrl.Result{}, nil
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

func (r *TenantReconciler) handleManagementState(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (*ctrl.Result, error) {
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
		// Correcting the annotation passes the MaasTenantConfig watch predicate.
		return &ctrl.Result{}, nil
	}

	return nil, nil
}

func (r *TenantReconciler) validateConfigAndGateway(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (*maasv1alpha1.Config, tenantreconcile.PlatformContext, *ctrl.Result, error) {
	mcfg, wait, err := r.readyConfigOrWait(ctx, log, tenant)
	if err != nil {
		return nil, tenantreconcile.PlatformContext{}, nil, err
	}
	if wait != nil {
		return nil, tenantreconcile.PlatformContext{}, wait, nil
	}

	if managementState(tenant.Annotations) == managementStateRemoved {
		log.V(1).Info("MaasTenantConfig in Removed management state with live Config; waiting for anchor teardown")
		if err := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "WaitingForRemovedTeardown",
			"management state is Removed; platform reconcile is suspended until the Config anchor is deleted by component GC"); err != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err
		}
		// Config deletion and a management-state change both pass the watch predicates.
		return nil, tenantreconcile.PlatformContext{}, &ctrl.Result{}, nil
	}

	fallbackGatewayRef := fallbackTenantGatewayRef(r.GatewayName, r.GatewayNamespace)
	platformContext, err := tenantreconcile.ResolvePlatformContext(ctx, r.Client, tenant, fallbackGatewayRef)
	if err != nil && !errors.Is(err, tenantreconcile.ErrPlatformContextNotReady) {
		return nil, tenantreconcile.PlatformContext{}, nil, r.recordFailure(ctx, tenant, "PlatformContextReadFailed", err)
	}
	if err != nil {
		if err2 := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "InvalidGateway", err.Error()); err2 != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err2
		}
		// Platform context comes from this tenant config and its AITenant; both are watched.
		return nil, tenantreconcile.PlatformContext{}, &ctrl.Result{}, nil
	}

	missingGateway, err := validateGatewayExists(ctx, r.Client, platformContext.GatewayRef.Namespace, platformContext.GatewayRef.Name)
	if err != nil {
		return nil, tenantreconcile.PlatformContext{}, nil, r.recordFailure(ctx, tenant, "GatewayCheckFailed", err)
	}
	if missingGateway != "" {
		log.Info("gateway validation failed", "reason", missingGateway)
		if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "GatewayNotReady", missingGateway); err2 != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err2
		}
		// The Gateway watch re-enqueues tenants that reference it once it is created.
		return nil, tenantreconcile.PlatformContext{}, &ctrl.Result{}, nil
	}

	if r.ManifestPath == "" {
		if err := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, "ManifestPathUnset",
			"MAAS_PLATFORM_MANIFESTS is not set and no default kustomize path resolved; cannot apply platform manifests"); err != nil {
			return nil, tenantreconcile.PlatformContext{}, nil, err
		}
		// ManifestPath is fixed at process start; only a restart can change it.
		return nil, tenantreconcile.PlatformContext{}, &ctrl.Result{}, nil
	}

	return mcfg, platformContext, nil, nil
}

func (r *TenantReconciler) checkDependenciesAndPrerequisites(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig) (tenantreconcile.PrerequisiteReport, *ctrl.Result, error) {
	missing, err := tenantreconcile.CheckDependencies(ctx, r.Client)
	if err != nil {
		return tenantreconcile.PrerequisiteReport{}, nil, r.recordFailure(ctx, tenant, "DependencyCheckFailed", err)
	}
	if missing != "" {
		setDependenciesCondition(tenant, false, missing)
		setDeploymentsAvailableCondition(tenant, false, "DependenciesNotMet", missing)
		prerequisitesUnevaluatedCondition(tenant, "Prerequisites were not evaluated because required dependencies are not met")
		if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "DependenciesNotAvailable", missing); err2 != nil {
			return tenantreconcile.PrerequisiteReport{}, nil, err2
		}
		// The CRD watch re-enqueues every tenant when the CRD appears; retryAfterDiscoveryLag
		// covers a CRD that is already Established.
		return tenantreconcile.PrerequisiteReport{}, &ctrl.Result{}, nil
	}
	setDependenciesCondition(tenant, true, "")

	appNs := r.appNamespaceForTenant()
	rep, err := tenantreconcile.CollectPrerequisiteReport(ctx, r.Client, appNs)
	if err != nil {
		return tenantreconcile.PrerequisiteReport{}, nil, r.recordFailure(ctx, tenant, "PrerequisiteCheckFailed", err)
	}
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
			return tenantreconcile.PrerequisiteReport{}, nil, err
		}
		// The only blocking prerequisite is the maas-db-config Secret, which is watched.
		return tenantreconcile.PrerequisiteReport{}, &ctrl.Result{}, nil
	}

	return rep, nil, nil
}

func (r *TenantReconciler) reconcilePlatform(
	ctx context.Context,
	log logr.Logger,
	tenant *maasv1alpha1.MaasTenantConfig,
	platformContext tenantreconcile.PlatformContext,
	mcfg *maasv1alpha1.Config,
) (*tenantreconcile.RunResult, *ctrl.Result, error) {
	appNs := r.appNamespaceForTenant()
	runRes, err := tenantreconcile.RunPlatform(ctx, log, r.Client, r.Scheme, tenant, platformContext, r.ManifestPath, appNs, r.ControllerNamespace, r.ClusterAudience, r.MonitoringNamespace, mcfg)
	if err != nil {
		// Any platform failure sets Ready=False. The reason tells a render that needs an edit
		// from an apply that is retrying.
		reason := "PlatformReconcileFailed"
		if errors.Is(err, reconcile.TerminalError(nil)) {
			reason = "PlatformRenderFailed"
		}
		setDeploymentsAvailableCondition(tenant, false, reason, err.Error())
		statusErr := r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, reason, err.Error())
		return nil, nil, fmt.Errorf("tenant platform reconcile: %w", failureAfterStatus(reason, statusErr, err))
	}

	r.kuadrantProbe.observe(types.NamespacedName{Namespace: platformContext.GatewayRef.Namespace, Name: platformContext.GatewayRef.Name}, runRes)

	if err := r.ensureGatewayManagementAuth(ctx, log, tenant); err != nil {
		return nil, nil, fmt.Errorf("ensure gateway management auth: %w", r.recordFailure(ctx, tenant, "GatewayAuthPolicyFailed", err))
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
			return nil, nil, err
		}
		// Rollout progress, EnvoyFilter changes and the payload-processing handshake
		// annotation on this tenant config all re-enqueue through watches.
		res := kuadrantRecheck(ctx, runRes)
		return nil, &res, nil
	}

	return runRes, nil, nil
}

func (r *TenantReconciler) aggregateWarningsAndSetDegraded(
	tenant *maasv1alpha1.MaasTenantConfig,
	prereqReport tenantreconcile.PrerequisiteReport,
	runRes *tenantreconcile.RunResult,
	usageLogsWarning string,
) {
	var allWarnings []string
	hasPrereqWarnings := len(prereqReport.Warnings) > 0
	hasPlatformWarnings := runRes != nil && len(runRes.Warnings) > 0
	hasKuadrantWarning := runRes != nil && runRes.KuadrantDetectionWarning != ""
	hasUsageLogsWarning := usageLogsWarning != ""
	hasRouterFallback := runRes != nil && runRes.KuadrantRouterFallback

	if hasPrereqWarnings {
		allWarnings = append(allWarnings, prereqReport.Warnings...)
	}
	if hasPlatformWarnings {
		allWarnings = append(allWarnings, runRes.Warnings...)
	}
	if hasKuadrantWarning {
		allWarnings = append(allWarnings, runRes.KuadrantDetectionWarning)
	}
	if hasUsageLogsWarning {
		allWarnings = append(allWarnings, usageLogsWarning)
	}
	if hasRouterFallback {
		allWarnings = append(allWarnings, kuadrantRouterFallbackWarning)
	}

	if len(allWarnings) > 0 {
		warningKinds := 0
		if hasPrereqWarnings {
			warningKinds++
		}
		if hasPlatformWarnings {
			warningKinds++
		}
		if hasKuadrantWarning {
			warningKinds++
		}
		if hasUsageLogsWarning {
			warningKinds++
		}
		if hasRouterFallback {
			warningKinds++
		}

		var reason string
		switch {
		case warningKinds > 1:
			reason = "MultipleWarnings"
		case hasPrereqWarnings:
			reason = "PrerequisitesWarning"
		case hasPlatformWarnings:
			reason = "InvalidReplicaAnnotation"
		case hasKuadrantWarning:
			reason = "KuadrantDetectionUnverified"
		case hasRouterFallback:
			reason = "KuadrantRouterFallback"
		default:
			reason = "UsageLoggingNotProvided"
		}
		setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionTrue,
			reason, strings.Join(allWarnings, "; "))
	} else {
		setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse, "NoWarnings", "")
	}
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

func (r *TenantReconciler) setFinalStatus(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig, runRes *tenantreconcile.RunResult) (ctrl.Result, error) {
	tenant.Status.Phase = "Active"
	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		tenant.Status.Phase = "Degraded"
	}
	setDeploymentsAvailableCondition(tenant, true, "DeploymentsReady", "maas-api deployment and payload-processing EnvoyFilter are available")
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

	// Operand drift re-enqueues through the operand watches; only the Kuadrant wasm
	// detection is polled.
	return kuadrantRecheck(ctx, runRes), nil
}

// retryAfterDiscoveryLag brings the reconcile back soon when it skipped a kind the REST
// mapper did not know although a CRD serving that kind is Established: discovery lags the
// Established condition and nothing reports it catching up. A kind with no such CRD waits
// for the CRD watch. CRDs are read from the cache the CRD watches keep, which the event
// that started this reconcile has already updated.
func (r *TenantReconciler) retryAfterDiscoveryLag(ctx context.Context, kinds []schema.GroupVersionKind, result ctrl.Result) ctrl.Result {
	if len(kinds) == 0 || (result.RequeueAfter > 0 && result.RequeueAfter <= discoveryLagRetry) {
		return result
	}
	var crds extv1.CustomResourceDefinitionList
	if err := r.List(ctx, &crds, client.UnsafeDisableDeepCopy); err != nil {
		oteljson.FromContext(ctx).Error(err, "failed to list CRDs for kinds skipped as not served", "kinds", kinds)
		return result
	}
	for i := range crds.Items {
		crd := &crds.Items[i]
		for _, gvk := range kinds {
			if crd.Spec.Group == gvk.Group && crd.Spec.Names.Kind == gvk.Kind && crdServesVersion(crd, gvk.Version) {
				return requeueOnTimer(ctx, discoveryLagRetry, "discovery does not serve "+gvk.Kind+" yet although its CRD is established")
			}
		}
	}
	return result
}

// readyConfigOrWait returns the singleton Config when it exists, is not deleting,
// and has a UID. Otherwise it updates MaasTenantConfig status and returns a Result the caller should return
// immediately without running gateway, dependency, prerequisite, or platform work.
func (r *TenantReconciler) readyConfigOrWait(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (*maasv1alpha1.Config, *ctrl.Result, error) {
	var ct maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &ct); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Config not found; skipping reconcile until it exists", "name", maasv1alpha1.ConfigInstanceName)
			if err2 := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "ConfigMissing",
				fmt.Sprintf("Config %q is required before platform apply", maasv1alpha1.ConfigInstanceName)); err2 != nil {
				return nil, nil, err2
			}
			// The Config watch admits its create event.
			return nil, &ctrl.Result{}, nil
		}
		return nil, nil, err
	}
	if !ct.DeletionTimestamp.IsZero() {
		log.Info("Config is terminating; skipping platform reconcile", "name", ct.Name)
		if err := r.patchStatus(ctx, tenant, "Pending", metav1.ConditionFalse, "ConfigTerminating",
			fmt.Sprintf("Config %q is deleting; platform reconcile is suspended until the anchor is gone or recreated", ct.Name)); err != nil {
			return nil, nil, err
		}
		// The Config watch admits the delete event and any recreate.
		return nil, &ctrl.Result{}, nil
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
func (r *TenantReconciler) handleIdleManagementState(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig, ms string) (ctrl.Result, error) {
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

// validateGatewayExists reports why the gateway cannot be used when it does not exist.
// An error means the read failed, which says nothing about the gateway.
func validateGatewayExists(ctx context.Context, c client.Client, namespace, name string) (missing string, err error) {
	gw := &gwapiv1.Gateway{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := c.Get(ctx, key, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("gateway %s/%s not found: the specified Gateway must exist before enabling MaaS platform reconcile", namespace, name), nil
		}
		return "", fmt.Errorf("failed to look up gateway %s/%s: %w", namespace, name, err)
	}
	return "", nil
}

// recordFailure records err in status under reason and returns it, so a failure shows in
// status and not only in the controller log. A terminal err sets Ready=False: the tenant
// config needs an edit. Any other err sets Degraded=True and leaves Ready alone, so a
// transient failure does not flap readiness for the AITenant and Lifecycle watches; the
// next complete pass clears Degraded.
func (r *TenantReconciler) recordFailure(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig, reason string, err error) error {
	if errors.Is(err, reconcile.TerminalError(nil)) {
		return failureAfterStatus(reason, r.patchStatus(ctx, tenant, "Failed", metav1.ConditionFalse, reason, err.Error()), err)
	}
	setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionTrue, reason, err.Error())
	return failureAfterStatus(reason, r.Status().Update(ctx, tenant), err)
}

// failureAfterStatus returns err unless recording it in status failed. Then only the status
// error stays in the chain: that write is always worth a retry, even after a terminal err.
func failureAfterStatus(reason string, statusErr, err error) error {
	if statusErr != nil {
		return fmt.Errorf("record %s in status: %w (after: %s)", reason, statusErr, err.Error())
	}
	return err
}

func (r *TenantReconciler) patchStatus(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig, phase string, status metav1.ConditionStatus, reason, message string) error {
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
func (r *TenantReconciler) cleanupTenantResources(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) error {
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
		{
			gvk:       tenantreconcile.GVKDeployment,
			name:      tenantreconcile.PayloadProcessingDeploymentName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKDeployment,
			name:      tenantreconcile.PayloadPreProcessingDeploymentName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKService,
			name:      tenantreconcile.PayloadProcessingServiceName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKService,
			name:      tenantreconcile.PayloadPreProcessingServiceName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKDestinationRule,
			name:      tenantreconcile.PayloadProcessingDeploymentName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKDestinationRule,
			name:      tenantreconcile.PayloadPreProcessingDeploymentName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKEnvoyFilter,
			name:      tenantreconcile.PayloadProcessingEnvoyFilterName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKEnvoyFilter,
			name:      tenantreconcile.UsageLogsEnvoyFilterName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKNetworkPolicy,
			name:      tenantreconcile.PayloadProcessingNetworkPolicyName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKServiceAccount,
			name:      tenantreconcile.PayloadProcessingServiceAccountName(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKConfigMap,
			name:      tenantreconcile.PayloadProcessingPluginsConfigMapForTenant(tenantID),
			namespace: gatewayNs,
		},
		{
			gvk:       tenantreconcile.GVKClusterRoleBinding,
			name:      tenantreconcile.PayloadProcessingReaderClusterRoleBindingNameForTenant(tenantID),
			namespace: "",
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
func (r *TenantReconciler) cleanupMaaSSubscriptions(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (bool, error) {
	log.Info("Cleaning up MaaSSubscription CRs", "namespace", tenant.Namespace)

	subscriptionList := &maasv1alpha1.MaaSSubscriptionList{}
	if err := r.List(ctx, subscriptionList, client.InNamespace(tenant.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list MaaSSubscriptions: %w", err)
	}

	for i := range subscriptionList.Items {
		subscription := &subscriptionList.Items[i]
		log.Info("Deleting MaaSSubscription", "name", subscription.Name, "namespace", subscription.Namespace)
		if err := r.Delete(ctx, subscription); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to delete MaaSSubscription %s/%s: %w", subscription.Namespace, subscription.Name, err)
		}
	}

	if len(subscriptionList.Items) > 0 {
		log.Info("Waiting for MaaSSubscription finalizer cleanup", "count", len(subscriptionList.Items))
		return false, nil
	}
	return true, nil
}

// cleanupMaaSAuthPolicies deletes all MaaSAuthPolicy CRs in the tenant namespace.
// MaaSAuthPolicyReconciler's handleDeletion will clean up the gateway AuthPolicy
// when the last MaaSAuthPolicy is deleted.
func (r *TenantReconciler) cleanupMaaSAuthPolicies(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) (bool, error) {
	log.Info("Cleaning up MaaSAuthPolicy CRs", "namespace", tenant.Namespace)

	// List all MaaSAuthPolicy CRs in this namespace
	policyList := &maasv1alpha1.MaaSAuthPolicyList{}
	if err := r.List(ctx, policyList, client.InNamespace(tenant.Namespace)); err != nil {
		return false, fmt.Errorf("failed to list MaaSAuthPolicies: %w", err)
	}

	// Delete each MaaSAuthPolicy
	for i := range policyList.Items {
		policy := &policyList.Items[i]
		log.Info("Deleting MaaSAuthPolicy", "name", policy.Name, "namespace", policy.Namespace)
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to delete MaaSAuthPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}

	if len(policyList.Items) > 0 {
		log.Info("Waiting for MaaSAuthPolicy finalizer cleanup", "count", len(policyList.Items))
		return false, nil
	}
	return true, nil
}

// ensureGatewayManagementAuth bootstraps maas-gateway-auth when missing so /maas-api/*
// management endpoints receive Authorino identity headers (RHOAIENG-81214).
func (r *TenantReconciler) ensureGatewayManagementAuth(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.MaasTenantConfig) error {
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return err
	}

	authR := &MaaSAuthPolicyReconciler{
		Client:                          r.Client,
		Scheme:                          r.Scheme,
		InfraNamespace:                  r.appNamespaceForTenant(),
		TenantNamespace:                 r.TenantNamespace,
		GatewayName:                     r.GatewayName,
		GatewayNamespace:                r.GatewayNamespace,
		ClusterAudience:                 r.ClusterAudience,
		MetadataCacheTTL:                r.MetadataCacheTTL,
		AuthzCacheTTL:                   r.MetadataCacheTTL,
		TenantNamespaceDiscoveryEnabled: r.TenantNamespaceDiscoveryEnabled,
	}

	gatewayNs, gatewayName, err := authR.fetchGatewayInfo(ctx, log, tenant.Namespace)
	if err != nil {
		return err
	}
	if tenantID != "" && gatewayNs == r.GatewayNamespace && gatewayName == r.GatewayName {
		return nil
	}

	return authR.ensureBaseGatewayAuthPolicy(
		ctx, log,
		authR.fetchOIDCConfig(ctx, log, tenant.Namespace),
		authR.discoverXAPIKeyNeeded(ctx, log),
		tenantID, gatewayNs, gatewayName,
	)
}
