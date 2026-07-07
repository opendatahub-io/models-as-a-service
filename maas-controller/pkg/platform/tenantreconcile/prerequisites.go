package tenantreconcile

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
func CollectPrerequisiteReport(ctx context.Context, c client.Client, appNamespace string) PrerequisiteReport {
	log := log.FromContext(ctx)
	var rep PrerequisiteReport

	if msg := checkAuthorinoTLS(ctx, c); msg != "" {
		rep.Warnings = append(rep.Warnings, msg)
		log.V(1).Info("MaaS prerequisite warning", "check", "authorino-tls", "message", msg)
	}
	if msg := checkDatabaseSecret(ctx, c, appNamespace); msg != "" {
		rep.Blocking = append(rep.Blocking, msg)
		log.Error(nil, "MaaS prerequisite error", "check", "database-secret", "message", msg)
	}
	if msg := checkDSCIMonitoring(ctx, c); msg != "" {
		rep.Warnings = append(rep.Warnings, msg)
		log.V(1).Info("MaaS prerequisite warning", "check", "dsci-monitoring", "message", msg)
	}

	return rep
}

// ValidatePrerequisites checks Tenant platform prerequisites (blocking + warnings).
// Warnings do not return an error; callers may surface them on status separately.
func ValidatePrerequisites(ctx context.Context, c client.Client, appNamespace string) error {
	rep := CollectPrerequisiteReport(ctx, c, appNamespace)
	if len(rep.Blocking) > 0 {
		all := append(append([]string{}, rep.Blocking...), rep.Warnings...)
		return fmt.Errorf("blocking prerequisites missing: %s", strings.Join(all, "; "))
	}
	return nil
}

func checkAuthorinoTLS(ctx context.Context, c client.Client) string {
	has, err := IsGVKAvailable(c, GVKAuthorino)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to check Authorino API availability")
		return "failed to check Authorino CRD availability due to a cluster API error"
	}
	if !has {
		return ""
	}

	authorinoList := &unstructured.UnstructuredList{}
	authorinoList.SetGroupVersionKind(gvkListKind(GVKAuthorino))
	if err := c.List(ctx, authorinoList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Authorino instances")
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
			log.FromContext(ctx).Error(err, "failed to read spec.listener.tls.enabled from Authorino", "name", item.GetName())
			continue
		}
		certName, _, err := unstructured.NestedString(item.Object, "spec", "listener", "tls", "certSecretRef", "name")
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to read spec.listener.tls.certSecretRef.name from Authorino", "name", item.GetName())
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
		log.FromContext(ctx).Error(err, "failed to check database Secret", "name", MaaSDBSecretName, "namespace", appNamespace)
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

func checkDSCIMonitoring(ctx context.Context, c client.Client) string {
	// Look for DSCInitialization resources
	dsciList := &unstructured.UnstructuredList{}
	dsciList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "dscinitialization.opendatahub.io",
		Version: "v1",
		Kind:    "DSCInitializationList",
	})

	if err := c.List(ctx, dsciList); err != nil {
		if meta.IsNoMatchError(err) {
			return "DSCI monitoring not configured: DSCInitialization CRD not found. " +
				"Showback/FinOps usage views will not work without monitoring stack enabled"
		}
		log.FromContext(ctx).Error(err, "unable to verify DSCI monitoring status")
		return "unable to verify DSCI monitoring status due to a cluster API error. " +
			"Ensure monitoring is enabled in DSCInitialization for showback functionality"
	}

	if len(dsciList.Items) == 0 {
		return "DSCI monitoring not configured: no DSCInitialization found. " +
			"Showback/FinOps usage views will not work without monitoring stack enabled"
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

	// Check MonitoringStackAvailable, MonitoringReady, and PersesAvailable conditions
	conditionsSlice, found, err := unstructured.NestedSlice(dsci.Object, "status", "conditions")
	if err != nil {
		log.FromContext(ctx).Error(err, "unable to read DSCI conditions")
		return "unable to verify DSCI monitoring conditions due to a status read error. " +
			"Ensure monitoring stack is deployed in DSCInitialization"
	}
	if !found || len(conditionsSlice) == 0 {
		return "DSCI monitoring status not available: no conditions found. " +
			"Monitoring stack may still be deploying. " +
			"Showback/FinOps usage views will not work until monitoring is ready"
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
				"Showback/FinOps usage views will not work until monitoring is ready"
		}
		msg := fmt.Sprintf("DSCI monitoring is not ready (MonitoringReady=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until monitoring is ready"
		return msg
	}

	if conditions["MonitoringStackAvailable"].status != "True" {
		cond := conditions["MonitoringStackAvailable"]
		if cond.status == "" {
			return "DSCI monitoring stack is not available: MonitoringStackAvailable condition not found in DSCInitialization status. " +
				"Showback/FinOps usage views will not work until monitoring stack is available"
		}
		msg := fmt.Sprintf("DSCI monitoring stack is not available (MonitoringStackAvailable=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until monitoring stack is available"
		return msg
	}

	if conditions["PersesAvailable"].status != "True" {
		cond := conditions["PersesAvailable"]
		if cond.status == "" {
			return "DSCI Perses is not available: PersesAvailable condition not found in DSCInitialization status. " +
				"Showback/FinOps usage views will not work until Perses is available"
		}
		msg := fmt.Sprintf("DSCI Perses is not available (PersesAvailable=%s", cond.status)
		if cond.reason != "" {
			msg += fmt.Sprintf(": %s", cond.reason)
		}
		if cond.message != "" {
			msg += fmt.Sprintf(": %s", cond.message)
		}
		msg += "). Showback/FinOps usage views will not work until Perses is available"
		return msg
	}

	return ""
}
