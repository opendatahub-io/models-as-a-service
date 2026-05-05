package maas

import (
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func setTenantCondition(tenant *maasv1alpha1.Tenant, typ string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func setDependenciesCondition(tenant *maasv1alpha1.Tenant, ok bool, detail string) {
	if ok {
		setTenantCondition(tenant, tenantreconcile.ConditionDependenciesAvailable, metav1.ConditionTrue,
			"DependenciesMet", "AuthConfig CRD (Kuadrant) is available on the cluster")
		return
	}
	setTenantCondition(tenant, tenantreconcile.ConditionDependenciesAvailable, metav1.ConditionFalse,
		"DependencyMissing", detail)
}

func setPrerequisiteConditionsFromReport(tenant *maasv1alpha1.Tenant, rep tenantreconcile.PrerequisiteReport) {
	switch {
	case len(rep.Blocking) > 0:
		agg := strings.Join(append(append([]string{}, rep.Blocking...), rep.Warnings...), "; ")
		setTenantCondition(tenant, tenantreconcile.ConditionMaaSPrerequisitesAvailable, metav1.ConditionFalse,
			"PrerequisitesMissing", agg)
		setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionTrue,
			"PrerequisitesMissing", agg)
	case len(rep.Warnings) > 0:
		agg := strings.Join(rep.Warnings, "; ")
		setTenantCondition(tenant, tenantreconcile.ConditionMaaSPrerequisitesAvailable, metav1.ConditionTrue,
			"PrerequisitesMet", "Prerequisites satisfied; see Degraded for warnings")
		setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionTrue,
			"PrerequisitesWarning", agg)
	default:
		setTenantCondition(tenant, tenantreconcile.ConditionMaaSPrerequisitesAvailable, metav1.ConditionTrue,
			"PrerequisitesMet", "All prerequisites are satisfied")
		setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse,
			"PrerequisitesMet", "All prerequisites are satisfied")
	}
}

func setDeploymentsAvailableCondition(tenant *maasv1alpha1.Tenant, ok bool, reason, message string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	setTenantCondition(tenant, tenantreconcile.ConditionDeploymentsAvailable, st, reason, message)
}

func prerequisitesUnevaluatedCondition(tenant *maasv1alpha1.Tenant, detail string) {
	setTenantCondition(tenant, tenantreconcile.ConditionMaaSPrerequisitesAvailable, metav1.ConditionUnknown,
		"DependenciesNotMet", detail)
	setTenantCondition(tenant, tenantreconcile.ConditionTypeDegraded, metav1.ConditionFalse,
		"DependenciesNotMet", detail)
}
