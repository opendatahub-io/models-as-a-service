/*
Copyright 2026.

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
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	usageLogsEnvoyFilterManifestPath = "/deployment/components/observability/usage-logs/dsci-managed/envoy-otel-access-log.yaml"

	// usageLogsCollectorService is the collector used when Config.spec.usageLogging is true.
	usageLogsCollectorService = "usage-logs-collector"
	// dataScienceUsageLogsCollectorService is the collector used when usageLogging is false or omitted.
	dataScienceUsageLogsCollectorService = "data-science-usage-logs-collector"
)

// usageLogsCollectorServiceName returns the collector Service name for the EnvoyFilter endpoint.
// usageLogging true keeps the historical collector. false and omitted use the data-science collector.
func usageLogsCollectorServiceName(mcfg *maasv1alpha1.Config) string {
	if mcfg != nil && ptr.Deref(mcfg.Spec.UsageLogging, false) {
		return usageLogsCollectorService
	}
	return dataScienceUsageLogsCollectorService
}

// tenantTelemetryEnabled matches tenantreconcile.isTelemetryEnabled for MaasTenantConfig.
func tenantTelemetryEnabled(tenant *maasv1alpha1.MaasTenantConfig) bool {
	t := tenant.Spec.Telemetry
	if t == nil || t.Enabled == nil {
		return false
	}
	return *t.Enabled
}

// deleteUsageLogsEnvoyFilterIfTelemetryDisabled removes the per-tenant filter when
// MaasTenantConfig.spec.telemetry is not enabled. Called before reconcile gates so a
// blocked tenant still stops exporting usage logs.
func (r *TenantReconciler) deleteUsageLogsEnvoyFilterIfTelemetryDisabled(
	ctx context.Context,
	log logr.Logger,
	tenant *maasv1alpha1.MaasTenantConfig,
) error {
	if tenantTelemetryEnabled(tenant) {
		return nil
	}
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return err
	}
	return r.deleteUsageLogsEnvoyFilterIfExists(ctx, log, tenantreconcile.UsageLogsEnvoyFilterName(tenantID))
}

// ensureUsageLogsEnvoyFilter deploys a per-tenant usage-logs EnvoyFilter in the gateway
// namespace when MaasTenantConfig.spec.telemetry is enabled, and deletes it otherwise.
// usageLogging selects the collector address and does not skip deployment.
//
// When the manifest or EnvoyFilter CRD is unavailable, a non-empty warning is returned
// so reconcile can surface Degraded on MaasTenantConfig.
// All other failures are returned as errors for the normal reconcile error path.
func (r *TenantReconciler) ensureUsageLogsEnvoyFilter(
	ctx context.Context,
	log logr.Logger,
	tenant *maasv1alpha1.MaasTenantConfig,
	platformContext tenantreconcile.PlatformContext,
	mcfg *maasv1alpha1.Config,
) (string, error) {
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return "", err
	}
	efName := tenantreconcile.UsageLogsEnvoyFilterName(tenantID)
	gatewayName := platformContext.GatewayRef.Name

	if !tenantTelemetryEnabled(tenant) {
		return "", r.deleteUsageLogsEnvoyFilterIfExists(ctx, log, efName)
	}

	if r.MonitoringNamespace == "" {
		log.V(1).Info("monitoring namespace not configured; skipping usage-logs EnvoyFilter")
		return "", nil
	}

	applied, err := r.applyUsageLogsEnvoyFilter(ctx, log, tenant, mcfg, efName, gatewayName)
	if err != nil {
		return "", err
	}
	if applied {
		return "", nil
	}
	return "Usage-logs EnvoyFilter not deployed: manifest or CRD not available", nil
}

func (r *TenantReconciler) deleteUsageLogsEnvoyFilterIfExists(ctx context.Context, log logr.Logger, efName string) error {
	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	ef.SetName(efName)
	ef.SetNamespace(r.GatewayNamespace)

	if err := r.Delete(ctx, ef); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to delete usage-logs EnvoyFilter %s: %w", efName, err)
	}
	log.Info("deleted usage-logs EnvoyFilter", "name", efName, "namespace", r.GatewayNamespace)
	return nil
}

func (r *TenantReconciler) applyUsageLogsEnvoyFilter(
	ctx context.Context,
	log logr.Logger,
	tenant *maasv1alpha1.MaasTenantConfig,
	mcfg *maasv1alpha1.Config,
	efName, gatewayName string,
) (bool, error) {
	manifestPath := usageLogsEnvoyFilterManifestPath
	if r.UsageLogsManifestPath != "" {
		manifestPath = filepath.Join(r.UsageLogsManifestPath, usageLogsDsciManagedKustomizeDir, "envoy-otel-access-log.yaml")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("EnvoyFilter manifest not found, skipping", "path", manifestPath)
			return false, nil
		}
		return false, fmt.Errorf("read EnvoyFilter manifest %s: %w", manifestPath, err)
	}

	ef := &unstructured.Unstructured{}
	dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	if _, _, err := dec.Decode(raw, nil, ef); err != nil {
		return false, fmt.Errorf("decode EnvoyFilter manifest: %w", err)
	}

	captureUser := tenant.Spec.Telemetry != nil &&
		tenant.Spec.Telemetry.Logs != nil &&
		ptr.Deref(tenant.Spec.Telemetry.Logs.CaptureUser, false)
	if captureUser {
		log.Info("WARNING: User identity usage logs enabled - ensure GDPR/privacy compliance", "field", "logs.captureUser", "value", true)
		if err := tenantreconcile.PatchUsageLogsUserID(ef); err != nil {
			return false, fmt.Errorf("patch user_id into EnvoyFilter: %w", err)
		}
	}

	collectorAddress := fmt.Sprintf("%s.%s.svc", usageLogsCollectorServiceName(mcfg), r.MonitoringNamespace)
	if err := tenantreconcile.PatchUsageLogsClusterAddress(ef, collectorAddress); err != nil {
		return false, fmt.Errorf("patch collector address in EnvoyFilter: %w", err)
	}

	if err := tenantreconcile.PatchUsageLogsEnvoyFilterWorkloadSelector(ef, gatewayName); err != nil {
		return false, fmt.Errorf("patch workloadSelector gateway in EnvoyFilter: %w", err)
	}

	// Attribute usage records to the per-tenant workload namespace, not the gateway
	// namespace the EnvoyFilter itself lives in.
	if err := tenantreconcile.PatchUsageLogsServiceNamespace(ef, tenant.GetNamespace()); err != nil {
		return false, fmt.Errorf("patch service.namespace in EnvoyFilter: %w", err)
	}

	ef.SetName(efName)
	ef.SetNamespace(r.GatewayNamespace)
	applyUsageLogsEnvoyFilterMetadata(ef, tenant)

	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(ef), client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
		if apimeta.IsNoMatchError(err) {
			log.Info("EnvoyFilter CRD not available, skipping usage-logs EnvoyFilter")
			return false, nil
		}
		return false, fmt.Errorf("apply usage-logs EnvoyFilter %s: %w", efName, err)
	}

	log.V(1).Info("applied usage-logs EnvoyFilter",
		"name", efName, "namespace", r.GatewayNamespace,
		"gateway", gatewayName, "collector", collectorAddress,
		"serviceNamespace", tenant.GetNamespace())
	return true, nil
}

func applyUsageLogsEnvoyFilterMetadata(obj client.Object, tenant *maasv1alpha1.MaasTenantConfig) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	if tenantLabels := tenant.GetLabels(); tenantLabels != nil {
		for _, key := range []string{
			"app.kubernetes.io/managed-by",
			"app.kubernetes.io/part-of",
			tenantreconcile.LabelManagedByAITenant,
			tenantreconcile.LabelAIGatewayTenant,
			tenantreconcile.LabelTenantName,
			tenantreconcile.LabelTenantNamespace,
			tenantreconcile.LabelGatewayAccess,
		} {
			if v := tenantLabels[key]; v != "" {
				labels[key] = v
			}
		}
	}
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if tenantAnnotations := tenant.GetAnnotations(); tenantAnnotations != nil {
		for _, key := range []string{
			tenantreconcile.AnnotationAITenantName,
			tenantreconcile.AnnotationAITenantNamespace,
		} {
			if v := tenantAnnotations[key]; v != "" {
				annotations[key] = v
			}
		}
	}
	obj.SetAnnotations(annotations)
}
