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
	"sigs.k8s.io/yaml"
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

// GetClusterServiceAccountIssuer returns spec.serviceAccountIssuer from OpenShift Authentication/cluster, or "".
func GetClusterServiceAccountIssuer(ctx context.Context, c client.Reader) (string, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "Authentication"})
	if err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, u); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	issuer, found, err := unstructured.NestedString(u.Object, "spec", "serviceAccountIssuer")
	if err != nil {
		return "", fmt.Errorf("reading spec.serviceAccountIssuer: %w", err)
	}
	if !found {
		return "", nil
	}
	return issuer, nil
}

func gvkListKind(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	out := gvk
	out.Kind = gvk.Kind + "List"
	return out
}

// PrerequisiteReport separates blocking errors from warnings (ODH modelsasservice validatePrerequisites parity).
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
	if msg := checkUserWorkloadMonitoring(ctx, c); msg != "" {
		rep.Warnings = append(rep.Warnings, msg)
		log.V(1).Info("MaaS prerequisite warning", "check", "user-workload-monitoring", "message", msg)
	}

	return rep
}

// ValidatePrerequisites mirrors modelsasservice validatePrerequisites (blocking + warnings).
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

func checkUserWorkloadMonitoring(ctx context.Context, c client.Client) string {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: MonitoringNamespace,
		Name:      ClusterMonitoringConfigName,
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			return "User Workload Monitoring not configured: ConfigMap 'cluster-monitoring-config' not found in 'openshift-monitoring'. " +
				"Showback/FinOps usage views will not work without User Workload Monitoring enabled"
		}
		log.FromContext(ctx).Error(err, "unable to verify User Workload Monitoring status")
		return "unable to verify User Workload Monitoring status due to a cluster API error. " +
			"Ensure User Workload Monitoring is enabled for showback functionality"
	}

	configData, ok := cm.Data["config.yaml"]
	if !ok {
		return "User Workload Monitoring is not enabled. " +
			"Set enableUserWorkload: true in 'cluster-monitoring-config' ConfigMap in 'openshift-monitoring' namespace. " +
			"Showback/FinOps usage views will not work without it"
	}

	var cfg struct {
		EnableUserWorkload bool `yaml:"enableUserWorkload"`
	}
	if err := yaml.Unmarshal([]byte(configData), &cfg); err != nil {
		return "User Workload Monitoring config is invalid in 'cluster-monitoring-config'. " +
			"Ensure config.yaml is valid YAML and sets enableUserWorkload: true"
	}

	if !cfg.EnableUserWorkload {
		return "User Workload Monitoring is not enabled. " +
			"Set enableUserWorkload: true in 'cluster-monitoring-config' ConfigMap in 'openshift-monitoring' namespace. " +
			"Showback/FinOps usage views will not work without it"
	}

	return ""
}
