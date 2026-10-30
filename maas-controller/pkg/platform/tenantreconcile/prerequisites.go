package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

// IsGVKAvailable uses the REST mapper (same spirit as ODH dependency checks).
func IsGVKAvailable(c client.Client, gvk schema.GroupVersionKind) (bool, error) {
	_, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func gvkListKind(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	out := gvk
	out.Kind = gvk.Kind + "List"
	return out
}

// PrerequisiteReport separates blocking errors from warnings.
type PrerequisiteReport struct {
	Blocking []string
	Warnings []string
}

// CollectPrerequisiteReport runs prerequisite checks and returns blocking vs warning messages.
// DSCI monitoring and usage-log checks run only when telemetryEnabled is true.
// usageMetricsDashboard gates the monitoring-stack check: a missing monitoring stack is a
// warning (and therefore Degraded) only when the legacy metrics dashboard is enabled.
// usageLogging selects which DSCInitialization usage-log conditions warn.
// When it is false, LokiStackAvailable, UsageLogsCollectorAvailable, or PersesAvailable
// not True is a warning. When it is true, only PersesAvailable not True is a warning.
func CollectPrerequisiteReport(ctx context.Context, c client.Client, appNamespace string, usageMetricsDashboard, usageLogging, telemetryEnabled bool) PrerequisiteReport {
	log := oteljson.FromContext(ctx)
	var rep PrerequisiteReport

	if msg := checkAuthorinoTLS(ctx, c); msg != "" {
		rep.Warnings = append(rep.Warnings, msg)
		log.V(1).Info("MaaS prerequisite warning", "check", "authorino-tls", "message", msg)
	}
	if msg := checkDatabaseSecret(ctx, c, appNamespace); msg != "" {
		rep.Blocking = append(rep.Blocking, msg)
		log.Error(nil, "MaaS prerequisite error", "check", "database-secret", "message", msg)
	}
	if telemetryEnabled {
		dsci, fetchErr := fetchDefaultDSCI(ctx, c)
		var conds map[string]dsciConditionStatus
		var condErr error
		if fetchErr == nil {
			conds, condErr = readDSCIConditionStatuses(ctx, dsci)
		}
		if usageMetricsDashboard {
			if msg := dsciMonitoringWarning(fetchErr, condErr, conds); msg != "" {
				rep.Warnings = append(rep.Warnings, msg)
				log.V(1).Info("MaaS prerequisite warning", "check", "dsci-monitoring", "message", msg)
			}
		}
		if msg := dsciUsageLoggingWarning(fetchErr, condErr, conds, usageLogging); msg != "" {
			rep.Warnings = append(rep.Warnings, msg)
			log.V(1).Info("MaaS prerequisite warning", "check", "dsci-usage-logging", "message", msg)
		}
	}

	return rep
}

// ValidatePrerequisites checks Tenant platform prerequisites (blocking + warnings).
// Warnings do not return an error; callers may surface them on status separately.
// usageMetricsDashboard, usageLogging, and telemetryEnabled gate DSCI warnings; see CollectPrerequisiteReport.
func ValidatePrerequisites(ctx context.Context, c client.Client, appNamespace string, usageMetricsDashboard, usageLogging, telemetryEnabled bool) error {
	rep := CollectPrerequisiteReport(ctx, c, appNamespace, usageMetricsDashboard, usageLogging, telemetryEnabled)
	if len(rep.Blocking) > 0 {
		all := append(append([]string{}, rep.Blocking...), rep.Warnings...)
		return fmt.Errorf("blocking prerequisites missing: %s", strings.Join(all, "; "))
	}
	return nil
}

func checkAuthorinoTLS(ctx context.Context, c client.Client) string {
	has, err := IsGVKAvailable(c, GVKAuthorino)
	if err != nil {
		oteljson.FromContext(ctx).Error(err, "failed to check Authorino API availability")
		return "failed to check Authorino CRD availability due to a cluster API error"
	}
	if !has {
		return ""
	}

	authorinoList := &unstructured.UnstructuredList{}
	authorinoList.SetGroupVersionKind(gvkListKind(GVKAuthorino))
	if err := c.List(ctx, authorinoList); err != nil {
		oteljson.FromContext(ctx).Error(err, "failed to list Authorino instances")
		return "failed to list Authorino instances due to a cluster API error"
	}

	if len(authorinoList.Items) == 0 {
		return "no Authorino instances found. " +
			"Authorino must be deployed and configured with TLS for MaaS authentication"
	}

	for i := range authorinoList.Items {
		item := &authorinoList.Items[i]
		enabled, _, err := unstructured.NestedBool(item.Object, "spec", "listener", "tls", "enabled")
		if err != nil {
			oteljson.FromContext(ctx).Error(err, "failed to read spec.listener.tls.enabled from Authorino", "name", item.GetName())
			continue
		}
		certName, _, err := unstructured.NestedString(item.Object, "spec", "listener", "tls", "certSecretRef", "name")
		if err != nil {
			oteljson.FromContext(ctx).Error(err, "failed to read spec.listener.tls.certSecretRef.name from Authorino", "name", item.GetName())
			continue
		}
		if enabled && certName != "" {
			return ""
		}
	}

	return "Authorino TLS is not configured: no Authorino instance has listener.tls.enabled=true with a certSecretRef. " +
		"Patch Authorino with spec.listener.tls.enabled=true and spec.listener.tls.certSecretRef to enable TLS. " +
		"See https://docs.kuadrant.io/1.0.x/authorino/docs/user-guides/mtls-authentication/"
}

func checkDatabaseSecret(ctx context.Context, c client.Client, appNamespace string) string {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: appNamespace,
		Name:      MaaSDBSecretName,
	}, secret)

	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("database Secret '%s' not found in namespace '%s'. "+
				"Create the Secret with key '%s' containing the PostgreSQL connection URL. "+
				"MaaS API cannot start without a database connection",
				MaaSDBSecretName, appNamespace, MaaSDBSecretKey)
		}
		oteljson.FromContext(ctx).Error(err, "failed to check database Secret", "name", MaaSDBSecretName, "namespace", appNamespace)
		return fmt.Sprintf("failed to check database Secret '%s' in namespace '%s' due to a cluster API error",
			MaaSDBSecretName, appNamespace)
	}

	value, ok := secret.Data[MaaSDBSecretKey]
	if !ok || strings.TrimSpace(string(value)) == "" {
		return fmt.Sprintf("database Secret '%s' in namespace '%s' is missing required key '%s'. "+
			"The Secret must contain a valid PostgreSQL connection URL",
			MaaSDBSecretName, appNamespace, MaaSDBSecretKey)
	}

	return ""
}

const (
	dsciConditionMonitoringReady             = "MonitoringReady"
	dsciConditionMonitoringStackAvailable    = "MonitoringStackAvailable"
	dsciConditionPersesAvailable             = "PersesAvailable"
	dsciConditionLokiStackAvailable          = "LokiStackAvailable"
	dsciConditionUsageLogsCollectorAvailable = "UsageLogsCollectorAvailable"
)

var (
	errDSCICRDMissing   = errors.New("DSCInitialization CRD not found")
	errDSCIListFailed   = errors.New("DSCInitialization list failed")
	errDSCINotFound     = errors.New("DSCInitialization not found")
	errDSCIStatusRead   = errors.New("DSCInitialization status read failed")
	errDSCINoConditions = errors.New("DSCInitialization has no conditions")
)

type dsciConditionStatus struct {
	status  string
	reason  string
	message string
}

func fetchDefaultDSCI(ctx context.Context, c client.Client) (*unstructured.Unstructured, error) {
	log := oteljson.FromContext(ctx)

	dsciList := &unstructured.UnstructuredList{}
	dsciList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "dscinitialization.opendatahub.io",
		Version: "v1",
		Kind:    "DSCInitializationList",
	})

	if err := c.List(ctx, dsciList); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, errDSCICRDMissing
		}
		log.Error(err, "unable to verify DSCI status")
		return nil, errDSCIListFailed
	}
	if len(dsciList.Items) == 0 {
		return nil, errDSCINotFound
	}

	// DSCInitialization is a singleton resource; prefer the well-known
	// "default-dsci" instance if present, otherwise fall back to the first item.
	dsci := &dsciList.Items[0]
	for i := range dsciList.Items {
		if dsciList.Items[i].GetName() == "default-dsci" {
			dsci = &dsciList.Items[i]
			break
		}
	}
	return dsci, nil
}

func readDSCIConditionStatuses(ctx context.Context, dsci *unstructured.Unstructured) (map[string]dsciConditionStatus, error) {
	conditionsSlice, found, err := unstructured.NestedSlice(dsci.Object, "status", "conditions")
	if err != nil {
		oteljson.FromContext(ctx).Error(err, "unable to read DSCI conditions")
		return nil, errDSCIStatusRead
	}
	if !found || len(conditionsSlice) == 0 {
		return nil, errDSCINoConditions
	}

	conditions := make(map[string]dsciConditionStatus, len(conditionsSlice))
	for _, cond := range conditionsSlice {
		condMap, ok := cond.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := condMap["type"].(string)
		if condType == "" {
			continue
		}
		status, _ := condMap["status"].(string)
		reason, _ := condMap["reason"].(string)
		message, _ := condMap["message"].(string)
		conditions[condType] = dsciConditionStatus{
			status:  status,
			reason:  reason,
			message: message,
		}
	}
	if len(conditions) == 0 {
		return nil, errDSCINoConditions
	}
	return conditions, nil
}

func dsciMonitoringWarning(fetchErr, condErr error, conditions map[string]dsciConditionStatus) string {
	switch {
	case errors.Is(fetchErr, errDSCICRDMissing):
		return "DSCI monitoring not configured: DSCInitialization CRD not found. " +
			"Showback/FinOps usage views will not work without monitoring stack enabled"
	case errors.Is(fetchErr, errDSCIListFailed):
		return "unable to verify DSCI monitoring status due to a cluster API error. " +
			"Ensure monitoring is enabled in DSCInitialization for showback functionality"
	case errors.Is(fetchErr, errDSCINotFound):
		return "DSCI monitoring not configured: no DSCInitialization found. " +
			"Showback/FinOps usage views will not work without monitoring stack enabled"
	case fetchErr != nil:
		return "unable to verify DSCI monitoring status due to a cluster API error. " +
			"Ensure monitoring is enabled in DSCInitialization for showback functionality"
	case errors.Is(condErr, errDSCIStatusRead):
		return "unable to verify DSCI monitoring conditions due to a status read error. " +
			"Ensure monitoring stack is deployed in DSCInitialization"
	case errors.Is(condErr, errDSCINoConditions):
		return "DSCI monitoring status not available: no conditions found. " +
			"Monitoring stack may still be deploying. " +
			"Showback/FinOps usage views will not work until monitoring is ready"
	case condErr != nil:
		return "unable to verify DSCI monitoring conditions due to a status read error. " +
			"Ensure monitoring stack is deployed in DSCInitialization"
	}

	if msg := monitoringConditionWarning(conditions, dsciConditionMonitoringReady,
		"DSCI monitoring is not ready: MonitoringReady condition not found in DSCInitialization status. "+
			"Showback/FinOps usage views will not work until monitoring is ready",
		"DSCI monitoring is not ready (MonitoringReady=%s",
		"Showback/FinOps usage views will not work until monitoring is ready"); msg != "" {
		return msg
	}
	if msg := monitoringConditionWarning(conditions, dsciConditionMonitoringStackAvailable,
		"DSCI monitoring stack is not available: MonitoringStackAvailable condition not found in DSCInitialization status. "+
			"Showback/FinOps usage views will not work until monitoring stack is available",
		"DSCI monitoring stack is not available (MonitoringStackAvailable=%s",
		"Showback/FinOps usage views will not work until monitoring stack is available"); msg != "" {
		return msg
	}
	return monitoringConditionWarning(conditions, dsciConditionPersesAvailable,
		"DSCI Perses is not available: PersesAvailable condition not found in DSCInitialization status. "+
			"Showback/FinOps usage views will not work until Perses is available",
		"DSCI Perses is not available (PersesAvailable=%s",
		"Showback/FinOps usage views will not work until Perses is available")
}

func monitoringConditionWarning(conditions map[string]dsciConditionStatus, name, missing, format, impact string) string {
	cond := conditions[name]
	if cond.status == "True" {
		return ""
	}
	if cond.status == "" {
		return missing
	}
	msg := fmt.Sprintf(format, cond.status)
	if cond.reason != "" {
		msg += fmt.Sprintf(": %s", cond.reason)
	}
	if cond.message != "" {
		msg += fmt.Sprintf(": %s", cond.message)
	}
	msg += "). " + impact
	return msg
}

// dsciUsageLoggingWarning reports platform usage-log dependencies.
// usageLogging true checks only PersesAvailable. false also checks LokiStackAvailable
// and UsageLogsCollectorAvailable.
func dsciUsageLoggingWarning(fetchErr, condErr error, conditions map[string]dsciConditionStatus, usageLogging bool) string {
	impact := "Logs-based usage views will not work until LokiStackAvailable, UsageLogsCollectorAvailable, and PersesAvailable are True"
	if usageLogging {
		impact = "Logs-based usage views will not work until PersesAvailable is True"
	}
	switch {
	case errors.Is(fetchErr, errDSCICRDMissing):
		return "DSCI usage logging not configured: DSCInitialization CRD not found. " + impact
	case errors.Is(fetchErr, errDSCINotFound):
		return "DSCI usage logging not configured: no DSCInitialization found. " + impact
	case fetchErr != nil:
		return "unable to verify DSCI usage logging status due to a cluster API error. " + impact
	case errors.Is(condErr, errDSCINoConditions):
		return "DSCI usage logging status not available: no conditions found. " + impact
	case condErr != nil:
		return "unable to verify DSCI usage logging conditions due to a status read error. " + impact
	}

	required := []string{
		dsciConditionLokiStackAvailable,
		dsciConditionUsageLogsCollectorAvailable,
		dsciConditionPersesAvailable,
	}
	if usageLogging {
		required = []string{dsciConditionPersesAvailable}
	}

	var failures []string
	for _, name := range required {
		cond := conditions[name]
		if cond.status == "True" {
			continue
		}
		if cond.status == "" {
			failures = append(failures, fmt.Sprintf(
				"DSCI %s is not available: %s condition not found in DSCInitialization status. %s",
				name, name, impact))
			continue
		}
		msg := fmt.Sprintf("DSCI %s is not available (%s=%s", name, name, cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). " + impact
		failures = append(failures, msg)
	}
	return strings.Join(failures, "; ")
}
