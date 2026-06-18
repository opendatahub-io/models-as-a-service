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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	componentsv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/components/v1alpha1"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const modelsAsServiceRepoURL = "https://github.com/opendatahub-io/models-as-a-service"

// ModelsAsServiceReconciler maintains the platform-facing aggregate MaaS module
// status while Tenant remains the runtime configuration and reconcile object.
// It assumes the platform creates the ModelsAsService singleton; standalone/dev
// installs may run without that CR and in that case this reconciler stays idle.
type ModelsAsServiceReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=modelsasservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=modelsasservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch

func (r *ModelsAsServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != componentsv1alpha1.ModelsAsServiceInstanceName {
		return ctrl.Result{}, nil
	}

	var module componentsv1alpha1.ModelsAsService
	if err := r.Get(ctx, req.NamespacedName, &module); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !module.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := module.DeepCopy()
	if err := r.reconcileStatus(ctx, &module); err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(original.Status, module.Status) {
		return ctrl.Result{}, nil
	}

	if err := r.Status().Patch(ctx, &module, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ModelsAsServiceReconciler) reconcileStatus(ctx context.Context, module *componentsv1alpha1.ModelsAsService) error {
	var tenants maasv1alpha1.TenantList
	if err := r.List(ctx, &tenants); err != nil {
		return err
	}

	summary := summarizeModuleTenants(tenants.Items)

	module.Status.ObservedGeneration = module.Generation
	module.Status.Releases = defaultModelsAsServiceReleases()
	module.Status.Phase = componentsv1alpha1.ModelsAsServicePhaseNotReady
	if summary.allReady() {
		module.Status.Phase = componentsv1alpha1.ModelsAsServicePhaseReady
	}

	setModuleCondition(&module.Status.Conditions, module.Generation, tenantreconcile.ReadyConditionType,
		readyConditionStatus(summary),
		readyConditionReason(summary),
		readyConditionMessage(summary),
	)
	setModuleCondition(&module.Status.Conditions, module.Generation, "ProvisioningSucceeded",
		provisioningConditionStatus(summary),
		provisioningConditionReason(summary),
		provisioningConditionMessage(summary),
	)
	setModuleCondition(&module.Status.Conditions, module.Generation, tenantreconcile.ConditionTypeDegraded,
		degradedConditionStatus(summary),
		degradedConditionReason(summary),
		degradedConditionMessage(summary),
	)

	return nil
}

func (r *ModelsAsServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(
			&componentsv1alpha1.ModelsAsService{},
			builder.WithPredicates(moduleSingletonPredicate()),
		).
		Watches(
			&maasv1alpha1.Tenant{},
			handler.EnqueueRequestsFromMapFunc(enqueueModelsAsServiceSingleton),
			builder.WithPredicates(defaultTenantSingletonPredicate()),
		).
		Complete(r)
}

type moduleTenantSummary struct {
	total         int
	notReady      []string
	unprovisioned []string
	degraded      []string
}

func (s moduleTenantSummary) allReady() bool {
	return s.total > 0 && len(s.notReady) == 0
}

func (s moduleTenantSummary) provisioningSucceeded() bool {
	return s.total > 0 && len(s.unprovisioned) == 0
}

func summarizeModuleTenants(tenants []maasv1alpha1.Tenant) moduleTenantSummary {
	summary := moduleTenantSummary{}

	for i := range tenants {
		tenant := tenants[i]
		if tenant.Name != maasv1alpha1.TenantInstanceName {
			continue
		}

		summary.total++

		if !tenant.DeletionTimestamp.IsZero() {
			msg := fmt.Sprintf("%s (being removed)", tenantDisplayName(&tenant))
			summary.notReady = append(summary.notReady, msg)
			summary.unprovisioned = append(summary.unprovisioned, msg)
			continue
		}

		ready := apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ReadyConditionType)
		degraded := apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
		provisioned := apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionDeploymentsAvailable)

		if !ready {
			summary.notReady = append(summary.notReady, tenantDisplayName(&tenant))
		}
		if !provisioned {
			summary.unprovisioned = append(summary.unprovisioned, tenantDisplayName(&tenant))
		}
		if degraded {
			summary.degraded = append(summary.degraded, tenantDisplayName(&tenant))
		}
	}

	sort.Strings(summary.notReady)
	sort.Strings(summary.unprovisioned)
	sort.Strings(summary.degraded)

	return summary
}

func tenantDisplayName(tenant *maasv1alpha1.Tenant) string {
	if tenant == nil {
		return ""
	}
	if tenant.Namespace != "" {
		return tenant.Namespace
	}
	return tenant.Name
}

func setModuleCondition(conditions *[]metav1.Condition, generation int64, typ string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	})
}

func defaultModelsAsServiceReleases() []componentsv1alpha1.ComponentRelease {
	return []componentsv1alpha1.ComponentRelease{
		{
			Name:    "Models as a Service",
			RepoURL: modelsAsServiceRepoURL,
		},
	}
}

func enqueueModelsAsServiceSingleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: componentsv1alpha1.ModelsAsServiceInstanceName},
	}}
}

func moduleSingletonPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == componentsv1alpha1.ModelsAsServiceInstanceName
	})
}

func defaultTenantSingletonPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == maasv1alpha1.TenantInstanceName
	})
}

func readyConditionStatus(summary moduleTenantSummary) metav1.ConditionStatus {
	if summary.allReady() {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func readyConditionReason(summary moduleTenantSummary) string {
	if summary.total == 0 {
		return "NoTenantsFound"
	}
	if summary.allReady() {
		return "AllTenantsReady"
	}
	return "TenantsNotReady"
}

func readyConditionMessage(summary moduleTenantSummary) string {
	if summary.total == 0 {
		return "MaaS is waiting for tenant status to be reported."
	}
	if summary.allReady() {
		return fmt.Sprintf("MaaS is ready in all %d tenant namespaces.", summary.total)
	}
	return fmt.Sprintf("MaaS is not ready in %d of %d tenant namespaces: %s",
		len(summary.notReady), summary.total, strings.Join(summary.notReady, "; "))
}

func provisioningConditionStatus(summary moduleTenantSummary) metav1.ConditionStatus {
	if summary.provisioningSucceeded() {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func provisioningConditionReason(summary moduleTenantSummary) string {
	if summary.total == 0 {
		return "NoTenantsFound"
	}
	if summary.provisioningSucceeded() {
		return "AllTenantsProvisioned"
	}
	return "TenantsProvisioningIncomplete"
}

func provisioningConditionMessage(summary moduleTenantSummary) string {
	if summary.total == 0 {
		return "MaaS is waiting for tenant setup status to be reported."
	}
	if summary.provisioningSucceeded() {
		return fmt.Sprintf("MaaS setup has completed in all %d tenant namespaces.", summary.total)
	}
	return fmt.Sprintf("MaaS setup is still in progress in %d of %d tenant namespaces: %s",
		len(summary.unprovisioned), summary.total, strings.Join(summary.unprovisioned, "; "))
}

func degradedConditionStatus(summary moduleTenantSummary) metav1.ConditionStatus {
	if len(summary.degraded) > 0 {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func degradedConditionReason(summary moduleTenantSummary) string {
	if len(summary.degraded) > 0 {
		return "TenantDegraded"
	}
	return "NoDegradedTenants"
}

func degradedConditionMessage(summary moduleTenantSummary) string {
	if len(summary.degraded) == 0 {
		if summary.total == 0 {
			return "MaaS is waiting for tenant status to be reported."
		}
		return fmt.Sprintf("MaaS is operating normally in all %d tenant namespaces.", summary.total)
	}
	return fmt.Sprintf("MaaS is degraded in %d of %d tenant namespaces: %s",
		len(summary.degraded), summary.total, strings.Join(summary.degraded, "; "))
}
