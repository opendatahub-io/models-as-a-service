package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

// IsGVKAvailable uses the REST mapper (same spirit as ODH dependency checks). A kind the
// mapper does not know is recorded through KindNotServed.
func IsGVKAvailable(ctx context.Context, c client.Client, gvk schema.GroupVersionKind) (bool, error) {
	_, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if KindNotServed(ctx, gvk, err) {
		return false, nil
	}
	if err != nil {
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

// CollectPrerequisiteReport runs prerequisite checks and returns blocking vs warning
// messages. An error means a check could not read the cluster, which says nothing about
// whether the prerequisite is met: the caller retries rather than wait for an event.
func CollectPrerequisiteReport(ctx context.Context, c client.Client, appNamespace string) (PrerequisiteReport, error) {
	log := oteljson.FromContext(ctx)
	var rep PrerequisiteReport

	authorinoMsg, authorinoErr := checkAuthorinoTLS(ctx, c)
	if authorinoMsg != "" {
		rep.Warnings = append(rep.Warnings, authorinoMsg)
		log.V(1).Info("MaaS prerequisite warning", "check", "authorino-tls", "message", authorinoMsg)
	}
	secretMsg, secretErr := checkDatabaseSecret(ctx, c, appNamespace)
	if secretMsg != "" {
		rep.Blocking = append(rep.Blocking, secretMsg)
		log.Error(nil, "MaaS prerequisite error", "check", "database-secret", "message", secretMsg)
	}
	dsciMsg, dsciErr := checkDSCIMonitoring(ctx, c)
	if dsciMsg != "" {
		rep.Warnings = append(rep.Warnings, dsciMsg)
		log.V(1).Info("MaaS prerequisite warning", "check", "dsci-monitoring", "message", dsciMsg)
	}

	return rep, errors.Join(authorinoErr, secretErr, dsciErr)
}

// ValidatePrerequisites checks Tenant platform prerequisites (blocking + warnings).
// Warnings do not return an error; callers may surface them on status separately.
func ValidatePrerequisites(ctx context.Context, c client.Client, appNamespace string) error {
	rep, err := CollectPrerequisiteReport(ctx, c, appNamespace)
	if err != nil {
		return fmt.Errorf("check prerequisites: %w", err)
	}
	if len(rep.Blocking) > 0 {
		all := append(append([]string{}, rep.Blocking...), rep.Warnings...)
		return fmt.Errorf("blocking prerequisites missing: %s", strings.Join(all, "; "))
	}
	return nil
}

func checkAuthorinoTLS(ctx context.Context, c client.Client) (string, error) {
	has, err := IsGVKAvailable(ctx, c, GVKAuthorino)
	if err != nil {
		return "", fmt.Errorf("check Authorino API availability: %w", err)
	}
	if !has {
		return "", nil
	}

	authorinoList := &unstructured.UnstructuredList{}
	authorinoList.SetGroupVersionKind(gvkListKind(GVKAuthorino))
	if err := c.List(ctx, authorinoList); err != nil {
		return "", fmt.Errorf("list Authorino instances: %w", err)
	}

	if len(authorinoList.Items) == 0 {
		return "no Authorino instances found. " +
			"Authorino must be deployed and configured with TLS for MaaS authentication", nil
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
			return "", nil
		}
	}

	return "Authorino TLS is not configured: no Authorino instance has listener.tls.enabled=true with a certSecretRef. " +
		"Patch Authorino with spec.listener.tls.enabled=true and spec.listener.tls.certSecretRef to enable TLS. " +
		"See https://docs.kuadrant.io/1.0.x/authorino/docs/user-guides/mtls-authentication/", nil
}

func checkDatabaseSecret(ctx context.Context, c client.Client, appNamespace string) (string, error) {
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
				MaaSDBSecretName, appNamespace, MaaSDBSecretKey), nil
		}
		return "", fmt.Errorf("read database Secret %s/%s: %w", appNamespace, MaaSDBSecretName, err)
	}

	value, ok := secret.Data[MaaSDBSecretKey]
	if !ok || strings.TrimSpace(string(value)) == "" {
		return fmt.Sprintf("database Secret '%s' in namespace '%s' is missing required key '%s'. "+
			"The Secret must contain a valid PostgreSQL connection URL",
			MaaSDBSecretName, appNamespace, MaaSDBSecretKey), nil
	}

	return "", nil
}

func checkDSCIMonitoring(ctx context.Context, c client.Client) (string, error) {
	log := oteljson.FromContext(ctx)

	// Look for DSCInitialization resources
	dsciGVK := schema.GroupVersionKind{Group: "dscinitialization.opendatahub.io", Version: "v1", Kind: "DSCInitialization"}
	dsciList := &unstructured.UnstructuredList{}
	dsciList.SetGroupVersionKind(gvkListKind(dsciGVK))

	if err := c.List(ctx, dsciList); err != nil {
		if KindNotServed(ctx, dsciGVK, err) {
			return "DSCI monitoring not configured: DSCInitialization CRD not found. " +
				"Showback/FinOps usage views will not work without monitoring stack enabled", nil
		}
		return "", fmt.Errorf("list DSCInitializations: %w", err)
	}

	if len(dsciList.Items) == 0 {
		return "DSCI monitoring not configured: no DSCInitialization found. " +
			"Showback/FinOps usage views will not work without monitoring stack enabled", nil
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

	msg, err := DSCIMonitoringWarning(dsci)
	if err != nil {
		log.Error(err, "unable to read DSCI conditions")
	}
	return msg, nil
}

// DSCIMonitoringWarning derives the prerequisite warning from a DSCInitialization's
// MonitoringStackAvailable, MonitoringReady, and PersesAvailable conditions. The warning
// is valid even when err is non-nil; err only explains an unreadable status.conditions
// for logging. The TenantReconciler DSCI watch compares the warning for the old and new
// object, so it must remain a pure function of the DSCI and the only DSCI data the
// tenant reconcile consumes.
func DSCIMonitoringWarning(dsci *unstructured.Unstructured) (string, error) {
	conditionsSlice, found, err := unstructured.NestedSlice(dsci.Object, "status", "conditions")
	if err != nil {
		return "unable to verify DSCI monitoring conditions due to a status read error. " +
			"Ensure monitoring stack is deployed in DSCInitialization", err
	}
	if !found || len(conditionsSlice) == 0 {
		return "DSCI monitoring status not available: no conditions found. " +
			"Monitoring stack may still be deploying. " +
			"Showback/FinOps usage views will not work until monitoring is ready", nil
	}

	type conditionStatus struct {
		status  string
		reason  string
		message string
	}

	conditions := map[string]conditionStatus{
		"MonitoringStackAvailable": {},
		"MonitoringReady":          {},
		"PersesAvailable":          {},
	}

	for _, cond := range conditionsSlice {
		condMap, ok := cond.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := condMap["type"].(string)
		if _, tracked := conditions[condType]; !tracked {
			continue
		}

		status, _ := condMap["status"].(string)
		reason, _ := condMap["reason"].(string)
		message, _ := condMap["message"].(string)

		conditions[condType] = conditionStatus{
			status:  status,
			reason:  reason,
			message: message,
		}
	}

	if conditions["MonitoringReady"].status != "True" {
		cond := conditions["MonitoringReady"]
		if cond.status == "" {
			return "DSCI monitoring is not ready: MonitoringReady condition not found in DSCInitialization status. " +
				"Showback/FinOps usage views will not work until monitoring is ready", nil
		}
		msg := fmt.Sprintf("DSCI monitoring is not ready (MonitoringReady=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until monitoring is ready"
		return msg, nil
	}

	if conditions["MonitoringStackAvailable"].status != "True" {
		cond := conditions["MonitoringStackAvailable"]
		if cond.status == "" {
			return "DSCI monitoring stack is not available: MonitoringStackAvailable condition not found in DSCInitialization status. " +
				"Showback/FinOps usage views will not work until monitoring stack is available", nil
		}
		msg := fmt.Sprintf("DSCI monitoring stack is not available (MonitoringStackAvailable=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until monitoring stack is available"
		return msg, nil
	}

	if conditions["PersesAvailable"].status != "True" {
		cond := conditions["PersesAvailable"]
		if cond.status == "" {
			return "DSCI Perses is not available: PersesAvailable condition not found in DSCInitialization status. " +
				"Showback/FinOps usage views will not work until Perses is available", nil
		}
		msg := fmt.Sprintf("DSCI Perses is not available (PersesAvailable=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until Perses is available"
		return msg, nil
	}

	return "", nil
}
