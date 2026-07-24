package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// RunResult is returned from Run for reconcile pacing.
type RunResult struct {
	DeploymentPending bool
	Detail            string
	// Warnings contains non-fatal issues discovered during reconciliation
	// (e.g. invalid replica-count annotations) that should be surfaced as status conditions.
	Warnings []string
}

// CheckDependencies verifies required CRDs (AuthConfig) are registered on the cluster.
func CheckDependencies(ctx context.Context, c client.Client) error {
	if ok, err := IsGVKAvailable(c, GVKAuthConfig); err != nil {
		return fmt.Errorf("dependencies: %w", err)
	} else if !ok {
		return errors.New("dependency missing: AuthConfig CRD (authorino.kuadrant.io/v1beta3) not available on cluster")
	}
	return nil
}

// RunPlatform runs kustomize render, apply, and deployment readiness after dependencies and prerequisites
// have succeeded and gateway ref is valid (caller validates gateway existence).
func RunPlatform(
	ctx context.Context,
	log logr.Logger,
	c client.Client,
	scheme *runtime.Scheme,
	tenant client.Object,
	platformContext PlatformContext,
	manifestPath string,
	appNs string,
	controllerNs string,
	clusterAudience string,
	mcfg *maasv1alpha1.Config,
) (*RunResult, error) {
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}

	if errs := validation.IsDNS1123Subdomain(appNs); len(errs) > 0 {
		return nil, fmt.Errorf("invalid application namespace %q: %v", appNs, errs)
	}

	if platformContext.GatewayRef.Namespace == "" || platformContext.GatewayRef.Name == "" {
		return nil, errors.New("gateway ref must be set before calling RunPlatform")
	}
	gw := &gwapiv1.Gateway{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: platformContext.GatewayRef.Namespace, Name: platformContext.GatewayRef.Name}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("gateway %s/%s not found", platformContext.GatewayRef.Namespace, platformContext.GatewayRef.Name)
		}
		return nil, fmt.Errorf("gateway lookup: %w", err)
	}

	params, err := BuildPlatformParams(tenant, platformContext, appNs, controllerNs, clusterAudience, log)
	if err != nil {
		return nil, fmt.Errorf("build params: %w", err)
	}

	rendered, err := RenderKustomize(manifestPath, appNs)
	if err != nil {
		return nil, fmt.Errorf("kustomize: %w", err)
	}

	resources, err := PostRender(ctx, log, tenant, rendered, params)
	if err != nil {
		return nil, fmt.Errorf("post-render: %w", err)
	}

	if err := ApplyRendered(ctx, c, scheme, tenant, appNs, mcfg, resources); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	if err := syncMaaSParametersConfigMap(ctx, c, appNs, params, log); err != nil {
		return nil, fmt.Errorf("sync maas-parameters ConfigMap: %w", err)
	}

	tenantID, err := TenantIdentifierFor(tenant)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant identifier: %w", err)
	}
	ready, detail, err := MaasAPIDeploymentReady(ctx, c, appNs, tenantID)
	if err != nil {
		return nil, fmt.Errorf("deployment status: %w", err)
	}
	if !ready {
		return &RunResult{DeploymentPending: true, Detail: detail, Warnings: params.Warnings}, nil
	}
	return &RunResult{Warnings: params.Warnings}, nil
}

// Run executes the Tenant platform pipeline (dependencies → prerequisites → render → apply → status).
// The application namespace is derived from the tenant config namespace.
func Run(
	ctx context.Context,
	log logr.Logger,
	c client.Client,
	scheme *runtime.Scheme,
	tenant client.Object,
	fallbackGatewayRef maasv1alpha1.TenantGatewayRef,
	manifestPath string,
	controllerNs string,
	clusterAudience string,
	mcfg *maasv1alpha1.Config,
) (*RunResult, error) {
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}

	if err := CheckDependencies(ctx, c); err != nil {
		return nil, err
	}

	appNs := tenant.GetNamespace()
	if errs := validation.IsDNS1123Subdomain(appNs); len(errs) > 0 {
		return nil, fmt.Errorf("invalid application namespace %q: %v", appNs, errs)
	}

	if err := ValidatePrerequisites(ctx, c, appNs); err != nil {
		return nil, fmt.Errorf("prerequisites: %w", err)
	}

	platformContext, err := ResolvePlatformContext(ctx, c, tenant, fallbackGatewayRef)
	if err != nil {
		return nil, err
	}

	return RunPlatform(ctx, log, c, scheme, tenant, platformContext, manifestPath, appNs, controllerNs, clusterAudience, mcfg)
}

const maasParametersConfigMapName = "maas-parameters"

// syncMaaSParametersConfigMap patches the maas-parameters ConfigMap with
// tenant-specific values. The RHOAI operator creates this ConfigMap with
// defaults from params.env; the maas-controller updates keys that the
// Tenant CR overrides (e.g., api-key-max-expiration-days).
func syncMaaSParametersConfigMap(ctx context.Context, c client.Client, namespace string, params PlatformParams, log logr.Logger) error {
	key := types.NamespacedName{Namespace: namespace, Name: maasParametersConfigMapName}

	// Quick check: skip if already correct (avoids unnecessary writes).
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(4).Info("maas-parameters ConfigMap not found, skipping sync")
			return nil
		}
		return fmt.Errorf("get maas-parameters ConfigMap: %w", err)
	}
	if cm.Data["api-key-max-expiration-days"] == params.APIKeyMaxExpirationDays {
		return nil
	}

	log.Info("Updating maas-parameters ConfigMap", "api-key-max-expiration-days", params.APIKeyMaxExpirationDays)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1.ConfigMap{}
		if err := c.Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.Data == nil {
			latest.Data = make(map[string]string)
		}
		latest.Data["api-key-max-expiration-days"] = params.APIKeyMaxExpirationDays
		return c.Update(ctx, latest)
	})
}

// MaasAPIDeploymentReady mirrors ODH deployments action for maas-api.
func MaasAPIDeploymentReady(ctx context.Context, c client.Client, appNamespace, tenantID string) (ready bool, detail string, err error) {
	dep := &appsv1.Deployment{}
	deploymentName := MaaSAPIDeploymentName(tenantID)
	key := types.NamespacedName{Namespace: appNamespace, Name: deploymentName}
	if err := c.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("deployment %s/%s not found", appNamespace, deploymentName), nil
		}
		return false, "", err
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Status.ObservedGeneration < dep.Generation {
		return false, "waiting for deployment spec to be observed", nil
	}
	if dep.Status.UpdatedReplicas < desired {
		return false, fmt.Sprintf("updated replicas %d/%d", dep.Status.UpdatedReplicas, desired), nil
	}
	if dep.Status.AvailableReplicas < desired {
		return false, fmt.Sprintf("available replicas %d/%d", dep.Status.AvailableReplicas, desired), nil
	}
	return true, "", nil
}
