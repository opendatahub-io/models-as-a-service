package maas

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	configPhaseReady    = "Ready"
	configPhaseNotReady = "Not Ready"

	conditionReady                 = "Ready"
	conditionProvisioningSucceeded = "ProvisioningSucceeded"
	conditionDegraded              = "Degraded"

	platformReleaseName = "platform"
)

func (r *TenantReconciler) syncConfigStatus(ctx context.Context, _ *maasv1alpha1.Config, tenant *maasv1alpha1.MaasTenantConfig) error {
	log := ctrl.LoggerFrom(ctx)

	var config maasv1alpha1.Config
	if err := r.Get(ctx, types.NamespacedName{Name: maasv1alpha1.ConfigInstanceName}, &config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading Config for status sync: %w", err)
	}
	if !config.DeletionTimestamp.IsZero() {
		return nil
	}

	ready := tenant.Status.Phase == "Active"

	config.Status.ObservedGeneration = config.Generation

	if ready {
		config.Status.Phase = configPhaseReady
	} else {
		config.Status.Phase = configPhaseNotReady
	}

	mirrorCondition(&config, tenant, conditionReady, tenantreconcile.ReadyConditionType, config.Generation)
	mirrorCondition(&config, tenant, conditionProvisioningSucceeded, tenantreconcile.ConditionDeploymentsAvailable, config.Generation)
	mirrorCondition(&config, tenant, conditionDegraded, tenantreconcile.ConditionTypeDegraded, config.Generation)

	if ready {
		platformVersion, err := r.getPlatformVersion(ctx)
		if err != nil {
			log.Error(err, "failed to read platform version ConfigMap; skipping version handshake")
		} else if platformVersion != "" {
			setPlatformRelease(&config, platformVersion)
		}
	}

	if err := r.Status().Update(ctx, &config); err != nil {
		return fmt.Errorf("updating Config status: %w", err)
	}

	return nil
}

func mirrorCondition(config *maasv1alpha1.Config, tenant *maasv1alpha1.MaasTenantConfig, targetType, sourceType string, generation int64) {
	src := apimeta.FindStatusCondition(tenant.Status.Conditions, sourceType)
	if src == nil {
		apimeta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               targetType,
			Status:             metav1.ConditionUnknown,
			Reason:             "ConditionNotReported",
			Message:            fmt.Sprintf("MaasTenantConfig has no %s condition", sourceType),
			ObservedGeneration: generation,
		})
		return
	}
	apimeta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               targetType,
		Status:             src.Status,
		Reason:             src.Reason,
		Message:            src.Message,
		ObservedGeneration: generation,
	})
}

func (r *TenantReconciler) getPlatformVersion(ctx context.Context) (string, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{
		Name:      tenantreconcile.PlatformConfigMapName,
		Namespace: r.AppNamespace,
	}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s ConfigMap: %w", tenantreconcile.PlatformConfigMapName, err)
	}
	return cm.Data[tenantreconcile.PlatformVersionKey], nil
}

func getPlatformRelease(config *maasv1alpha1.Config) *maasv1alpha1.ComponentRelease {
	for i := range config.Status.Releases {
		if config.Status.Releases[i].Name == platformReleaseName {
			return &config.Status.Releases[i]
		}
	}
	return nil
}

func setPlatformRelease(config *maasv1alpha1.Config, version string) {
	if r := getPlatformRelease(config); r != nil {
		r.Version = version
		return
	}
	config.Status.Releases = append(config.Status.Releases, maasv1alpha1.ComponentRelease{
		Name:    platformReleaseName,
		Version: version,
	})
}
