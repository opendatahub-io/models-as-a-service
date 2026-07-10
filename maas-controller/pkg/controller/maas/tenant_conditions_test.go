package maas

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func newTestTenant() *maasv1alpha1.Tenant {
	return &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "default-tenant", Namespace: "test-ns"},
	}
}

func TestSetPrerequisiteConditionsFromReport_NoIssues(t *testing.T) {
	tenant := newTestTenant()
	rep := tenantreconcile.PrerequisiteReport{}

	setPrerequisiteConditionsFromReport(tenant, rep)

	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionMaaSPrerequisitesAvailable) {
		t.Error("expected MaaSPrerequisitesAvailable=True when no issues")
	}
	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		t.Error("expected Degraded=False when no issues")
	}
}

func TestSetPrerequisiteConditionsFromReport_BlockingOnly(t *testing.T) {
	tenant := newTestTenant()
	rep := tenantreconcile.PrerequisiteReport{
		Blocking: []string{"database secret missing"},
	}

	setPrerequisiteConditionsFromReport(tenant, rep)

	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionMaaSPrerequisitesAvailable) {
		t.Error("expected MaaSPrerequisitesAvailable=False when blocking")
	}
	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		t.Error("expected Degraded=True when blocking")
	}
}

func TestSetPrerequisiteConditionsFromReport_WarningsOnly(t *testing.T) {
	tenant := newTestTenant()
	rep := tenantreconcile.PrerequisiteReport{
		Warnings: []string{"authorino TLS not configured"},
	}

	setPrerequisiteConditionsFromReport(tenant, rep)

	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionMaaSPrerequisitesAvailable) {
		t.Error("expected MaaSPrerequisitesAvailable=True when warnings only")
	}
	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		t.Error("expected Degraded=True when warnings present")
	}
}

func TestSetPrerequisiteConditionsFromReport_InformationalOnly(t *testing.T) {
	tenant := newTestTenant()
	rep := tenantreconcile.PrerequisiteReport{
		Informational: []string{"DSCI monitoring not configured"},
	}

	setPrerequisiteConditionsFromReport(tenant, rep)

	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionMaaSPrerequisitesAvailable) {
		t.Error("expected MaaSPrerequisitesAvailable=True when informational only")
	}
	if apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		t.Error("expected Degraded=False when only informational messages present")
	}
	cond := apimeta.FindStatusCondition(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
	if cond == nil {
		t.Fatal("expected Degraded condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected Degraded status=False, got %s", cond.Status)
	}
}

func TestSetPrerequisiteConditionsFromReport_WarningsAndInformational(t *testing.T) {
	tenant := newTestTenant()
	rep := tenantreconcile.PrerequisiteReport{
		Warnings:      []string{"authorino TLS not configured"},
		Informational: []string{"DSCI monitoring not configured"},
	}

	setPrerequisiteConditionsFromReport(tenant, rep)

	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionMaaSPrerequisitesAvailable) {
		t.Error("expected MaaSPrerequisitesAvailable=True")
	}
	if !apimeta.IsStatusConditionTrue(tenant.Status.Conditions, tenantreconcile.ConditionTypeDegraded) {
		t.Error("expected Degraded=True when warnings present (regardless of informational)")
	}
}
