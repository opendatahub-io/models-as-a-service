package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	monitoringNamespace string,
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

	params, err := BuildPlatformParams(tenant, platformContext, appNs, controllerNs, clusterAudience, monitoringNamespace, log)
	if err != nil {
		return nil, fmt.Errorf("build params: %w", err)
	}

	if !params.SkipIPP {
		wasmPresent, err := gatewayHasKuadrantWasmAuth(ctx, c, platformContext.GatewayRef.Namespace, platformContext.GatewayRef.Name)
		if err != nil {
			return nil, fmt.Errorf("detect gateway kuadrant wasm: %w", err)
		}
		params.PayloadProcessingRouterExtProcFallback = !wasmPresent
		if params.PayloadProcessingRouterExtProcFallback {
			log.Info("Kuadrant WASM auth not found on gateway; enabling ext_proc router fallback patches",
				"gateway", platformContext.GatewayRef.Namespace+"/"+platformContext.GatewayRef.Name)
		}
	}

	rendered, err := RenderKustomize(manifestPath, appNs)
	if err != nil {
		return nil, fmt.Errorf("kustomize: %w", err)
	}

	resources, err := PostRender(ctx, log, tenant, rendered, params)
	if err != nil {
		return nil, fmt.Errorf("post-render: %w", err)
	}

	// SSA only creates/updates resources in the rendered set; it does NOT delete
	// absent resources. When SkipIPP is true (praxis), run a one-shot, ownership-
	// gated cleanup of legacy maas-controller IPP operands, then stop touching
	// payload-processing names — ai-gateway-controller owns them for praxis tenants.
	if params.SkipIPP {
		if !isIPPMigrationCleanupComplete(tenant) {
			if err := cleanupIPPResources(ctx, c, params, mcfg.UID, log); err != nil {
				return nil, fmt.Errorf("cleanup IPP resources: %w", err)
			}
			if err := markIPPMigrationCleanupComplete(ctx, c, tenant); err != nil {
				return nil, fmt.Errorf("mark IPP migration cleanup complete: %w", err)
			}
		}
	} else {
		if err := clearIPPMigrationCleanupComplete(ctx, c, tenant); err != nil {
			return nil, fmt.Errorf("clear IPP migration cleanup marker: %w", err)
		}
		if err := cleanupPayloadProcessingHPA(ctx, c, params, log); err != nil {
			return nil, fmt.Errorf("cleanup payload-processing HPA: %w", err)
		}
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
	if !params.SkipIPP {
		ready, detail, err = PayloadProcessingEnvoyFilterReady(ctx, c, params.GatewayNamespace, params.GatewayName, tenantID)
		if err != nil {
			return nil, fmt.Errorf("payload-processing EnvoyFilter status: %w", err)
		}
		if !ready {
			return &RunResult{DeploymentPending: true, Detail: detail, Warnings: params.Warnings}, nil
		}
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
	monitoringNamespace string,
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

	return RunPlatform(ctx, log, c, scheme, tenant, platformContext, manifestPath, appNs, controllerNs, clusterAudience, monitoringNamespace, mcfg)
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

// PayloadProcessingEnvoyFilterReady verifies the per-tenant gateway EnvoyFilter that
// wires ext_proc is present with a priority high enough to run after Kuadrant's wasm
// insert. Without that, RHCL body-routed inference returns 404 NR on that gateway.
//
// This is a config-shape check (not a live config_dump). Use
// scripts/check-payload-ext-proc-filters.sh to confirm filters are in the proxy.
func PayloadProcessingEnvoyFilterReady(ctx context.Context, c client.Client, gatewayNamespace, gatewayName, tenantID string) (ready bool, detail string, err error) {
	efName := PayloadProcessingEnvoyFilterName(tenantID)
	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(GVKEnvoyFilter)
	key := types.NamespacedName{Namespace: gatewayNamespace, Name: efName}
	if err := c.Get(ctx, key, ef); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf(
				"EnvoyFilter %s/%s not found — ext_proc will not run; body-routed /v1/* returns 404 NR",
				gatewayNamespace, efName), nil
		}
		return false, "", err
	}

	priority, found, err := unstructured.NestedInt64(ef.Object, "spec", "priority")
	if err != nil {
		return false, "", fmt.Errorf("read EnvoyFilter priority: %w", err)
	}
	if !found || priority < PayloadProcessingEnvoyFilterPriority {
		shown := "missing"
		if found {
			shown = strconv.FormatInt(priority, 10)
		}
		return false, fmt.Sprintf(
			"EnvoyFilter %s/%s spec.priority=%s; need >= %d so HTTP_FILTER inserts apply after Kuadrant wasm (otherwise body-routed /v1/* returns 404 NR)",
			gatewayNamespace, efName, shown, PayloadProcessingEnvoyFilterPriority), nil
	}

	// Istio 1.26+: targetRefs and workloadSelector are mutually exclusive. MaaS
	// EnvoyFilters use workloadSelector keyed by gateway-name (see params patch).
	wsLabels, found, err := unstructured.NestedStringMap(ef.Object, "spec", "workloadSelector", "labels")
	if err != nil {
		return false, "", fmt.Errorf("read EnvoyFilter workloadSelector: %w", err)
	}
	const gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
	if !found || wsLabels[gatewayNameLabel] == "" {
		return false, fmt.Sprintf(
			"EnvoyFilter %s/%s has no workloadSelector.labels[%q]",
			gatewayNamespace, efName, gatewayNameLabel), nil
	}
	if got := wsLabels[gatewayNameLabel]; got != gatewayName {
		return false, fmt.Sprintf(
			"EnvoyFilter %s/%s workloadSelector.labels[%q]=%q; expected gateway %q",
			gatewayNamespace, efName, gatewayNameLabel, got, gatewayName), nil
	}
	return true, "", nil
}

const aiGatewayControllerFieldOwner = "ai-gateway-controller"

func isIPPMigrationCleanupComplete(tenant client.Object) bool {
	annotations := tenant.GetAnnotations()
	return annotations != nil && annotations[AnnotationIPPMigrationCleanupComplete] == "true"
}

func markIPPMigrationCleanupComplete(ctx context.Context, c client.Client, tenant client.Object) error {
	return patchTenantAnnotations(ctx, c, tenant, func(annotations map[string]string) {
		annotations[AnnotationIPPMigrationCleanupComplete] = "true"
	})
}

func clearIPPMigrationCleanupComplete(ctx context.Context, c client.Client, tenant client.Object) error {
	if !isIPPMigrationCleanupComplete(tenant) {
		return nil
	}
	return patchTenantAnnotations(ctx, c, tenant, func(annotations map[string]string) {
		delete(annotations, AnnotationIPPMigrationCleanupComplete)
	})
}

func patchTenantAnnotations(ctx context.Context, c client.Client, tenant client.Object, mutate func(map[string]string)) error {
	key := client.ObjectKeyFromObject(tenant)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, ok := tenant.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("expected client.Object copy, got %T", tenant.DeepCopyObject())
		}
		if err := c.Get(ctx, key, latest); err != nil {
			return err
		}
		base, ok := latest.DeepCopyObject().(client.Object)
		if !ok {
			return fmt.Errorf("expected client.Object copy, got %T", latest.DeepCopyObject())
		}
		annotations := latest.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		mutate(annotations)
		latest.SetAnnotations(annotations)
		return c.Patch(ctx, latest, client.MergeFrom(base))
	})
}

func hasSSAFieldManager(obj *unstructured.Unstructured, manager string) bool {
	managedFields, found, err := unstructured.NestedSlice(obj.Object, "metadata", "managedFields")
	if err != nil || !found {
		return false
	}
	for _, entry := range managedFields {
		field, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if mgr, _ := field["manager"].(string); mgr == manager {
			return true
		}
	}
	return false
}

func hasConfigControllerOwner(obj *unstructured.Unstructured, configUID types.UID) bool {
	if configUID == "" {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind != "Config" || ref.UID != configUID {
			continue
		}
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// isMaaSOwnedIPPResource reports whether obj is legacy IPP applied by maas-controller
// and safe to delete during the one-shot IPP→praxis migration cleanup. Resources owned
// by ai-gateway-controller (praxis) or with opendatahub.io/managed=false are excluded.
func isMaaSOwnedIPPResource(obj *unstructured.Unstructured, configUID types.UID) bool {
	if ann := obj.GetAnnotations(); ann != nil && ann[AnnotationManaged] == "false" {
		return false
	}
	if hasSSAFieldManager(obj, aiGatewayControllerFieldOwner) {
		return false
	}
	if hasConfigControllerOwner(obj, configUID) {
		return true
	}
	if hasSSAFieldManager(obj, ssaFieldOwner) {
		return true
	}
	labels := obj.GetLabels()
	if labels != nil && labels[LabelTenantName] != "" {
		return true
	}
	return false
}

func setConfigControllerOwnerRef(obj *unstructured.Unstructured, configUID types.UID) {
	if configUID == "" {
		return
	}
	controller := true
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "maas.opendatahub.io/v1alpha1",
		Kind:       "Config",
		Name:       maasv1alpha1.ConfigInstanceName,
		UID:        configUID,
		Controller: &controller,
	}})
}

type ippResourceRef struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

// ippResourcesForTenant lists IPP operands maas-controller may have created for a tenant.
// When SkipIPP is true these are omitted from the rendered set; explicit deletion is
// required because SSA apply does not remove absent resources (see cleanupPayloadProcessingHPA).
func ippResourcesForTenant(params PlatformParams) []ippResourceRef {
	tenantID := params.TenantIdentifier
	gatewayNamespace := params.GatewayNamespace
	return []ippResourceRef{
		{gvk: GVKHPA, namespace: gatewayNamespace, name: PayloadProcessingHPAName(tenantID)},
		{gvk: GVKDeployment, namespace: gatewayNamespace, name: PayloadProcessingDeploymentName(tenantID)},
		{gvk: GVKDeployment, namespace: gatewayNamespace, name: PayloadPreProcessingDeploymentName(tenantID)},
		{gvk: GVKService, namespace: gatewayNamespace, name: PayloadProcessingServiceName(tenantID)},
		{gvk: GVKService, namespace: gatewayNamespace, name: PayloadPreProcessingServiceName(tenantID)},
		{gvk: GVKEnvoyFilter, namespace: gatewayNamespace, name: PayloadProcessingEnvoyFilterName(tenantID)},
		{gvk: GVKNetworkPolicy, namespace: gatewayNamespace, name: PayloadProcessingNetworkPolicyName(tenantID)},
		{gvk: GVKDestinationRule, namespace: gatewayNamespace, name: PayloadProcessingDeploymentName(tenantID)},
		{gvk: GVKDestinationRule, namespace: gatewayNamespace, name: PayloadPreProcessingDeploymentName(tenantID)},
		{gvk: GVKServiceAccount, namespace: gatewayNamespace, name: PayloadProcessingServiceAccountName(tenantID)},
		{gvk: GVKConfigMap, namespace: gatewayNamespace, name: PayloadProcessingPluginsConfigMapForTenant(tenantID)},
		{gvk: GVKClusterRoleBinding, name: PayloadProcessingReaderClusterRoleBindingNameForTenant(tenantID)},
	}
}

// cleanupIPPResources removes legacy maas-controller IPP operands during the one-shot
// IPP→praxis migration. Only maas-owned resources are deleted; praxis operands applied
// by ai-gateway-controller at the same names are left intact.
func cleanupIPPResources(ctx context.Context, c client.Client, params PlatformParams, configUID types.UID, log logr.Logger) error {
	for _, ref := range ippResourcesForTenant(params) {
		if err := deleteIPPResourceIfManaged(ctx, c, ref, configUID, log); err != nil {
			return err
		}
	}
	return nil
}

func deleteIPPResourceIfManaged(ctx context.Context, c client.Client, ref ippResourceRef, configUID types.UID, log logr.Logger) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ref.gvk)
	obj.SetName(ref.name)
	obj.SetNamespace(ref.namespace)

	key := client.ObjectKeyFromObject(obj)
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get %s %s/%s: %w", ref.gvk.Kind, ref.namespace, ref.name, err)
	}

	if !isMaaSOwnedIPPResource(obj, configUID) {
		log.V(1).Info("Skipping IPP cleanup for resource not owned by maas-controller",
			"kind", ref.gvk.Kind, "name", ref.name, "namespace", ref.namespace)
		return nil
	}

	log.Info("Deleting legacy IPP resource during praxis migration",
		"kind", ref.gvk.Kind, "name", ref.name, "namespace", ref.namespace)
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", ref.gvk.Kind, ref.namespace, ref.name, err)
	}
	return nil
}

// cleanupPayloadProcessingHPA deletes the payload-processing HPA when autoscaling
// is disabled. This is necessary because SSA only creates/updates resources but never
// deletes resources that are no longer in the rendered set. Without explicit cleanup,
// an HPA created during an autoscaling-enabled reconcile would remain active after
// the autoscaling annotation is removed, continuing to scale pods.
func cleanupPayloadProcessingHPA(ctx context.Context, c client.Client, params PlatformParams, log logr.Logger) error {
	if params.PayloadProcessingAutoscaling {
		// Autoscaling is enabled; the HPA is (being) created, nothing to clean up.
		return nil
	}

	tenantID := params.TenantIdentifier
	hpaName := PayloadProcessingHPAName(tenantID)
	hpa := &unstructured.Unstructured{}
	hpa.SetGroupVersionKind(GVKHPA)
	key := types.NamespacedName{Namespace: params.GatewayNamespace, Name: hpaName}

	if err := c.Get(ctx, key, hpa); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // No HPA exists, nothing to clean up.
		}
		return fmt.Errorf("get HPA %s/%s: %w", params.GatewayNamespace, hpaName, err)
	}

	log.Info("Deleting orphaned payload-processing HPA (autoscaling disabled)",
		"hpa", hpaName, "namespace", params.GatewayNamespace)
	if err := c.Delete(ctx, hpa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete HPA %s/%s: %w", params.GatewayNamespace, hpaName, err)
	}
	return nil
}
