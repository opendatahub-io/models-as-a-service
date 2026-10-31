package maas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// +kubebuilder:rbac:groups=monitoring.rhobs,resources=prometheusrules,verbs=get;list;watch;create;patch;delete

// ensureAuthorinoMonitoring connects Authorino to the DSCI monitoring stack.
// The coreos ServiceMonitor is selected by OTEL; rhobs rules are evaluated by
// the product Prometheus against OTEL-normalized metric names.
func (r *LifecycleReconciler) ensureAuthorinoMonitoring(ctx context.Context, log logr.Logger) error {
	if r.AuthorinoMonitoringManifestsPath == "" {
		return nil
	}
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		return client.IgnoreNotFound(err)
	}
	resources, err := authorinoMonitoringResources(r.AuthorinoMonitoringManifestsPath, r.MonitoringNamespace)
	if err != nil {
		return err
	}
	if !ptr.Deref(cfg.Spec.AuthorinoMonitoring, true) {
		return r.deleteOwnedRenderedResources(ctx, log, cfg, resources, "authorinoMonitoring")
	}
	for _, resource := range resources {
		res := resource.DeepCopy()
		if err := controllerutil.SetControllerReference(&cfg, res, r.Scheme); err != nil {
			return fmt.Errorf("set Authorino monitoring owner: %w", err)
		}
		if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(res), client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
			// COO supplies monitoring.rhobs; core OpenShift supplies monitoring.coreos.com.
			if isOptionalAPIGroup(res.GroupVersionKind().Group) && (apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err)) {
				continue
			}
			return fmt.Errorf("apply Authorino %s %s/%s: %w", res.GetKind(), res.GetNamespace(), res.GetName(), err)
		}
	}
	return nil
}

func authorinoMonitoringResources(manifestsPath, namespace string) ([]unstructured.Unstructured, error) {
	files := []string{
		"authorino-maas-metadata-evaluator-prometheusrule.yaml",
		"authorino-maas-oidc-prometheusrule.yaml",
		"authorino-server-metrics-servicemonitor.yaml",
	}
	resources := make([]unstructured.Unstructured, 0, len(files))
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(manifestsPath, name))
		if err != nil {
			return nil, fmt.Errorf("read Authorino manifest %s: %w", name, err)
		}
		var resource unstructured.Unstructured
		if err := yaml.Unmarshal(data, &resource.Object); err != nil {
			return nil, fmt.Errorf("decode Authorino manifest %s: %w", name, err)
		}
		resource.SetNamespace(namespace)
		resources = append(resources, resource)
	}
	return resources, nil
}
