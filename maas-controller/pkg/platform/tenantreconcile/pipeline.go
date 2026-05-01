package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// RunResult is returned from Run for reconcile pacing.
type RunResult struct {
	DeploymentPending bool
	Detail            string
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
func RunPlatform(ctx context.Context, log logr.Logger, c client.Client, scheme *runtime.Scheme, tenant *maasv1alpha1.Tenant, manifestPath string, appNs string) (*RunResult, error) {
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}

	if errs := validation.IsDNS1123Subdomain(appNs); len(errs) > 0 {
		return nil, fmt.Errorf("invalid application namespace %q: %v", appNs, errs)
	}

	if tenant.Spec.GatewayRef.Namespace == "" || tenant.Spec.GatewayRef.Name == "" {
		return nil, errors.New("gateway ref must be set (reconciler should default gateway before calling RunPlatform)")
	}
	gw := &gwapiv1.Gateway{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: tenant.Spec.GatewayRef.Namespace, Name: tenant.Spec.GatewayRef.Name}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("gateway %s/%s not found", tenant.Spec.GatewayRef.Namespace, tenant.Spec.GatewayRef.Name)
		}
		return nil, fmt.Errorf("gateway lookup: %w", err)
	}

	audience, err := GetClusterServiceAccountIssuer(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("cluster audience: %w", err)
	}
	if err := CustomizeParams(manifestPath, tenant, appNs, audience); err != nil {
		return nil, fmt.Errorf("customize params: %w", err)
	}

	rendered, err := RenderKustomize(manifestPath, appNs)
	if err != nil {
		return nil, fmt.Errorf("kustomize: %w", err)
	}

	resources, err := PostRender(ctx, log, tenant, rendered)
	if err != nil {
		return nil, fmt.Errorf("post-render: %w", err)
	}

	if err := ApplyRendered(ctx, c, scheme, tenant, resources); err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	ready, detail, err := MaasAPIDeploymentReady(ctx, c, appNs)
	if err != nil {
		return nil, fmt.Errorf("deployment status: %w", err)
	}
	if !ready {
		return &RunResult{DeploymentPending: true, Detail: detail}, nil
	}
	return &RunResult{}, nil
}

// Run executes the ODH-equivalent modelsasservice pipeline against Tenant.
// The application namespace is derived from tenant.Namespace (Tenant CR is co-located with workloads).
func Run(ctx context.Context, log logr.Logger, c client.Client, scheme *runtime.Scheme, tenant *maasv1alpha1.Tenant, manifestPath string) (*RunResult, error) {
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("manifest path: %w", err)
	}

	if err := CheckDependencies(ctx, c); err != nil {
		return nil, err
	}

	appNs := tenant.Namespace
	if errs := validation.IsDNS1123Subdomain(appNs); len(errs) > 0 {
		return nil, fmt.Errorf("invalid application namespace %q: %v", appNs, errs)
	}

	if err := ValidatePrerequisites(ctx, c, appNs); err != nil {
		return nil, fmt.Errorf("prerequisites: %w", err)
	}

	return RunPlatform(ctx, log, c, scheme, tenant, manifestPath, appNs)
}

// MaasAPIDeploymentReady mirrors ODH deployments action for maas-api.
func MaasAPIDeploymentReady(ctx context.Context, c client.Client, appNamespace string) (ready bool, detail string, err error) {
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: appNamespace, Name: MaaSAPIDeploymentName}
	if err := c.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("deployment %s/%s not found", appNamespace, MaaSAPIDeploymentName), nil
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
