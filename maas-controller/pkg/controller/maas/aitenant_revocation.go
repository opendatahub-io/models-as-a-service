package maas

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	aitenantAPIKeysRevokedReasonJobCompleted        = "RevocationJobCompleted"
	aitenantAPIKeysRevokedReasonOperandsUnavailable = "RevocationOperandsUnavailable"

	// maasTenantConfigRevocationOperandsRequestedAnnotation nudges MaasTenantConfig reconcile so the
	// tenant controller can re-apply maas-api operands before the revocation Job runs.
	maasTenantConfigRevocationOperandsRequestedAnnotation = "maas.opendatahub.io/revocation-operands-requested"
)

// prepareRevocationOperands ensures maas-api and the cleanup ServiceAccount exist before
// creating the revocation Job. When operands cannot be restored, revocation is skipped
// with an explicit status reason (not equivalent to a successful maas-api revoke call).
func (r *AITenantReconciler) prepareRevocationOperands(ctx context.Context, aitenant *maasv1alpha1.AITenant) (bool, error) {
	if r.revocationOperandsReady(ctx, aitenant) {
		return true, nil
	}

	recoverable, err := r.revocationOperandsRecoverable(ctx)
	if err != nil {
		return false, err
	}
	if !recoverable {
		if _, err := r.skipTenantAPIKeysRevocation(ctx, aitenant,
			"revocation operands are unavailable and cannot be restored"); err != nil {
			return false, err
		}
		return true, nil
	}

	log := oteljson.FromContext(ctx)
	if err := r.requestRevocationOperandsViaMaasTenantConfig(ctx, aitenant); err != nil {
		return false, err
	}
	if r.revocationOperandsReady(ctx, aitenant) {
		return true, nil
	}

	tenantNamespace := r.tenantNamespaceName(aitenant)
	var tenant maasv1alpha1.MaasTenantConfig
	tenantKey := types.NamespacedName{
		Namespace: tenantNamespace,
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
	}
	if err := r.Get(ctx, tenantKey, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			if _, err := r.skipTenantAPIKeysRevocation(ctx, aitenant,
				"MaasTenantConfig is not available to restore revocation operands"); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, fmt.Errorf("get MaasTenantConfig while waiting for revocation operands: %w", err)
	}

	// A terminating MaasTenantConfig can never restore the operands: its own cleanup
	// defers until this AITenant reports API-key revocation. Waiting on it deadlocks
	// deletion whenever both are removed together (for example the Config
	// owner-reference cascade during teardown), so treat it like a missing owner and
	// skip revocation explicitly instead of requeueing forever.
	if !tenant.DeletionTimestamp.IsZero() {
		if _, err := r.skipTenantAPIKeysRevocation(ctx, aitenant,
			"MaasTenantConfig is terminating and cannot restore revocation operands"); err != nil {
			return false, err
		}
		return true, nil
	}

	log.Info("revocation operands still not ready after MaasTenantConfig reconcile; requeueing",
		"aitenant", aitenant.Namespace+"/"+aitenant.Name)
	return false, nil
}

func (r *AITenantReconciler) revocationOperandsReady(ctx context.Context, aitenant *maasv1alpha1.AITenant) bool {
	if strings.TrimSpace(r.AppNamespace) == "" {
		return false
	}
	tenantID := tenantIDForRevocation(aitenant)

	var sa corev1.ServiceAccount
	if err := r.get(ctx, types.NamespacedName{Namespace: r.AppNamespace, Name: aitenantAPIKeyCleanupServiceAccountName}, &sa); err != nil {
		return false
	}

	var svc corev1.Service
	if err := r.get(ctx, types.NamespacedName{Namespace: r.AppNamespace, Name: tenantreconcile.MaaSAPIServiceName(tenantID)}, &svc); err != nil {
		return false
	}

	var dep appsv1.Deployment
	if err := r.get(ctx, types.NamespacedName{Namespace: r.AppNamespace, Name: tenantreconcile.MaaSAPIDeploymentName(tenantID)}, &dep); err != nil {
		return false
	}
	return dep.Status.AvailableReplicas > 0
}

func tenantIDForRevocation(aitenant *maasv1alpha1.AITenant) string {
	tenantID := aitenant.Name
	if tenantID == tenantreconcile.DefaultAITenantName {
		return ""
	}
	return tenantID
}

func (r *AITenantReconciler) revocationOperandsRecoverable(ctx context.Context) (bool, error) {
	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Config for revocation recovery: %w", err)
	}
	return cfg.DeletionTimestamp.IsZero(), nil
}

func (r *AITenantReconciler) requestRevocationOperandsViaMaasTenantConfig(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	tenantNamespace := r.tenantNamespaceName(aitenant)
	var tenant maasv1alpha1.MaasTenantConfig
	tenantKey := types.NamespacedName{
		Namespace: tenantNamespace,
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
	}
	if err := r.Get(ctx, tenantKey, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MaasTenantConfig for revocation operand restore request: %w", err)
	}
	if !tenant.DeletionTimestamp.IsZero() {
		return nil
	}

	marker := revocationOperandsRequestMarker(aitenant)
	if marker == "" {
		return errors.New("cannot request revocation operand restore without AITenant UID")
	}
	if tenant.Annotations != nil &&
		tenant.Annotations[maasTenantConfigRevocationOperandsRequestedAnnotation] == marker {
		return nil
	}

	base := tenant.DeepCopy()
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[maasTenantConfigRevocationOperandsRequestedAnnotation] = marker
	if err := r.Patch(ctx, &tenant, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("request MaasTenantConfig reconcile for revocation operands: %w", err)
	}
	return nil
}

// revocationOperandsRequestMarker identifies one AITenant deletion so recovery nudges are idempotent.
func revocationOperandsRequestMarker(aitenant *maasv1alpha1.AITenant) string {
	if aitenant.UID == "" {
		return ""
	}
	if aitenant.DeletionTimestamp != nil && !aitenant.DeletionTimestamp.IsZero() {
		return fmt.Sprintf("%s/%s", aitenant.UID, aitenant.DeletionTimestamp.UTC().Format(time.RFC3339))
	}
	return string(aitenant.UID)
}

func (r *AITenantReconciler) skipTenantAPIKeysRevocation(ctx context.Context, aitenant *maasv1alpha1.AITenant, message string) (bool, error) {
	log := oteljson.FromContext(ctx)
	log.Info("skipping tenant API-key revocation", "aitenant", aitenant.Namespace+"/"+aitenant.Name, "reason", message)
	if r.Recorder != nil {
		r.Recorder.Eventf(aitenant, nil, corev1.EventTypeWarning, "APIKeyCleanupSkipped", "InvalidateAPIKeys",
			"Skipping API-key cleanup for AITenant %s/%s: %s", aitenant.Namespace, aitenant.Name, message)
	}
	if err := r.markTenantAPIKeysRevocationSkipped(ctx, aitenant, message); err != nil {
		return false, err
	}
	return true, nil
}

func (r *AITenantReconciler) markTenantAPIKeysRevocationSkipped(ctx context.Context, aitenant *maasv1alpha1.AITenant, message string) error {
	statusSnapshot := aitenant.Status.DeepCopy()
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               aitenantAPIKeysRevokedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             aitenantAPIKeysRevokedReasonOperandsUnavailable,
		Message:            message,
		ObservedGeneration: aitenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.updateAITenantStatus(ctx, aitenant, statusSnapshot); err != nil {
		return fmt.Errorf("mark skipped API-key revocation on AITenant %s/%s: %w", aitenant.Namespace, aitenant.Name, err)
	}
	return nil
}
