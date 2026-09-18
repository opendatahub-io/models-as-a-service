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
	// gated cleanup of existing MaaS-owned IPP operands, then stop touching
	// payload-processing names — ai-gateway-controller owns them for praxis tenants.
	if params.SkipIPP {
		switch payloadProcessingStatus(tenant) {
		case PayloadProcessingStatusCleanupComplete:
			// Already signaled; do not re-cleanup.
		case "":
			// Absent: legacy still owned the names — clean up, then signal.
			if err := cleanupIPPResources(ctx, c, params, mcfg.UID, log); err != nil {
				return nil, fmt.Errorf("cleanup IPP resources: %w", err)
			}
			if err := markPayloadProcessingCleanupComplete(ctx, c, tenant); err != nil {
				return nil, fmt.Errorf("mark payload-processing cleanup complete: %w", err)
			}
		default:
			// Any other value is a peer claim; do not overwrite it.
		}
	} else {
		// Legacy IPP is selected. Status drives the handshake (no bundleExists gate):
		//   absent            → apply
		//   cleanup-complete  → CAS-claim to absent, then apply
		//   any other value   → wait for peer switch-off
		ready, err := ensureLegacyMayDeploy(ctx, c, tenant)
		if err != nil {
			return nil, fmt.Errorf("ensure legacy may deploy: %w", err)
		}
		if !ready {
			return &RunResult{
				DeploymentPending: true,
				Detail:            "waiting for the praxis payload-processing cleanup to finish before redeploying legacy IPP",
				Warnings:          params.Warnings,
			}, nil
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

const ippExternalModelManagedBy = "ipp-external-model-reconciler"

const ippExternalModelLabel = "inference.opendatahub.io/external-model"

var inferenceExternalModelRouteOwnerGVK = schema.GroupVersionKind{Group: "inference.opendatahub.io", Version: "v1alpha1", Kind: "ExternalModel"}

func payloadProcessingStatus(tenant client.Object) string {
	annotations := tenant.GetAnnotations()
	if annotations == nil {
		return ""
	}
	return annotations[AnnotationPayloadProcessingStatus]
}

func isPayloadProcessingCleanupComplete(tenant client.Object) bool {
	return payloadProcessingStatus(tenant) == PayloadProcessingStatusCleanupComplete
}

func markPayloadProcessingCleanupComplete(ctx context.Context, c client.Client, tenant client.Object) error {
	return patchTenantAnnotations(ctx, c, tenant, func(annotations map[string]string) {
		annotations[AnnotationPayloadProcessingStatus] = PayloadProcessingStatusCleanupComplete
	})
}

// ensureLegacyMayDeploy decides whether legacy IPP may apply for tenant.
//
//	absent           → ready
//	cleanup-complete → CAS-claim to absent, then ready
//	any other value  → wait (peer owns the dataplane)
func ensureLegacyMayDeploy(ctx context.Context, c client.Client, tenant client.Object) (ready bool, err error) {
	switch payloadProcessingStatus(tenant) {
	case "":
		return true, nil
	case PayloadProcessingStatusCleanupComplete:
		return claimLegacySteady(ctx, c, tenant)
	default:
		return false, nil
	}
}

// legacyIPPBundleExists reports whether this tenant's legacy IPP Deployment is
// already present. Kept for tests / diagnostics; the deploy gate no longer
// uses it (status is the durable claim).
func legacyIPPBundleExists(ctx context.Context, c client.Client, params PlatformParams) (bool, error) {
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: params.GatewayNamespace, Name: PayloadProcessingDeploymentName(params.TenantIdentifier)}
	if err := c.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get legacy IPP deployment %s/%s: %w", key.Namespace, key.Name, err)
	}
	return true, nil
}

// claimLegacySteady atomically consumes cleanup-complete by deleting the
// status annotation (legacy steady = absent) via optimistic-concurrency Update.
func claimLegacySteady(ctx context.Context, c client.Client, tenant client.Object) (claimed bool, err error) {
	latest, ok := tenant.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("expected client.Object copy, got %T", tenant.DeepCopyObject())
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(tenant), latest); err != nil {
		return false, fmt.Errorf("get tenant config for payload-processing status claim: %w", err)
	}
	if payloadProcessingStatus(latest) != PayloadProcessingStatusCleanupComplete {
		return false, nil
	}
	annotations := latest.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	delete(annotations, AnnotationPayloadProcessingStatus)
	latest.SetAnnotations(annotations)
	if err := c.Update(ctx, latest); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim payload-processing status to legacy steady: %w", err)
	}
	return true, nil
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

// isMaaSOwnedIPPResource reports whether obj is safe to delete during the one-shot
// Praxis enablement cleanup. Resources owned by ai-gateway-controller are excluded.
//
// opendatahub.io/managed=false does NOT block cleanup: that annotation only opts a
// resource out of steady-state SSA (see ApplyRendered). Backend switch-off must
// still remove the whole IPP name set — including the plugins ConfigMap maas
// stamps managed=false on — so the other controller can recreate it in its own
// schema. Unmanaged leftovers (and MaaS-owned operands) are therefore deleted;
// only cross-controller praxis ownership is preserved as a race guard.
func isMaaSOwnedIPPResource(obj *unstructured.Unstructured, configUID types.UID) bool {
	if hasSSAFieldManager(obj, aiGatewayControllerFieldOwner) {
		return false
	}
	if ann := obj.GetAnnotations(); ann != nil && ann[AnnotationManaged] == "false" {
		return true
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

// cleanupIPPResources removes maas-controller IPP operands during one-shot Praxis
// enablement. Only MaaS-owned resources are deleted; ai-gateway-controller resources
// at the same names are left intact.
func cleanupIPPResources(ctx context.Context, c client.Client, params PlatformParams, configUID types.UID, log logr.Logger) error {
	for _, ref := range ippResourcesForTenant(params) {
		if err := deleteIPPResourceIfManaged(ctx, c, ref, configUID, log); err != nil {
			return err
		}
	}
	if err := ensureIPPWritersStopped(ctx, c, params, configUID); err != nil {
		return err
	}
	if err := cleanupIPPExternalModelRoutes(ctx, c, params, log); err != nil {
		return err
	}
	return nil
}

func ensureIPPWritersStopped(ctx context.Context, c client.Client, params PlatformParams, configUID types.UID) error {
	// The pinned IPP entrypoint enables its ExternalModel reconciler unless
	// DISABLE_EXTERNAL_MODEL_CONTROLLER=true. MaaS does not claim to observe
	// that flag; it proves the writer is stopped by observing its owned
	// Deployment and tenant-scoped pods disappear before route cleanup.
	var writerChecks []ippResourceRef
	for _, ref := range ippResourcesForTenant(params) {
		if ref.gvk != GVKDeployment {
			continue
		}
		writerChecks = append(writerChecks, ref)
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ref.gvk)
		if err := c.Get(ctx, client.ObjectKey{Namespace: ref.namespace, Name: ref.name}, obj); err == nil {
			if isMaaSOwnedIPPResource(obj, configUID) {
				if obj.GetDeletionTimestamp() != nil {
					return fmt.Errorf("IPP writer %s/%s is terminating; defer IPP-owned HTTPRoute cleanup", ref.namespace, ref.name)
				}
				return fmt.Errorf("IPP writer %s/%s is still present; defer IPP-owned HTTPRoute cleanup", ref.namespace, ref.name)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("check IPP writer %s/%s: %w", ref.namespace, ref.name, err)
		}
	}
	for _, ref := range writerChecks {
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace(ref.namespace), client.MatchingLabels{LabelTenantInstance: ref.name}); err != nil {
			return fmt.Errorf("check IPP writer pods for %s/%s: %w", ref.namespace, ref.name, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			labels := pod.GetLabels()
			if labels["app.kubernetes.io/managed-by"] == aiGatewayControllerFieldOwner {
				continue
			}
			app := labels["app"]
			if app == "" {
				return fmt.Errorf("IPP writer pod %s/%s has ambiguous identity; defer IPP-owned HTTPRoute cleanup", pod.Namespace, pod.Name)
			}
			if app == PayloadProcessingName || app == PayloadPreProcessingName {
				return fmt.Errorf("IPP writer pod %s/%s is still present; defer IPP-owned HTTPRoute cleanup", pod.Namespace, pod.Name)
			}
		}
	}
	return nil
}

// cleanupIPPExternalModelRoutes removes only routes created by the IPP ExternalModel
// reconciler. Route names alone do not prove ownership. ModelNamespace is the
// tenant namespace containing ExternalModels and their routes; AppNamespace may
// be shared infrastructure and must not be searched by this cleanup. Foreground
// deletion of the IPP Deployments completes before this runs, ensuring their
// in-process route writer has stopped.
func cleanupIPPExternalModelRoutes(ctx context.Context, c client.Client, params PlatformParams, log logr.Logger) error {
	if params.ModelNamespace == "" {
		return errors.New("model namespace is required for IPP-owned HTTPRoute cleanup")
	}
	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(GVKHTTPRoute.GroupVersion().WithKind("HTTPRouteList"))
	if err := c.List(ctx, routes, client.InNamespace(params.ModelNamespace)); err != nil {
		return fmt.Errorf("list existing IPP HTTPRoutes in %s: %w", params.ModelNamespace, err)
	}
	for i := range routes.Items {
		route := &routes.Items[i]
		labels := route.GetLabels()
		if labels["app.kubernetes.io/managed-by"] != ippExternalModelManagedBy {
			continue
		}
		modelName := labels[ippExternalModelLabel]
		if modelName == "" || hasSSAFieldManager(route, aiGatewayControllerFieldOwner) {
			continue
		}
		owner := controllerOwner(route, inferenceExternalModelRouteOwnerGVK, modelName)
		if owner == nil || owner.UID == "" {
			log.Info("Skipping ambiguous IPP HTTPRoute cleanup", "name", route.GetName(), "namespace", route.GetNamespace())
			continue
		}
		inference := &unstructured.Unstructured{}
		inference.SetGroupVersionKind(inferenceExternalModelRouteOwnerGVK)
		if err := c.Get(ctx, client.ObjectKey{Namespace: params.ModelNamespace, Name: modelName}, inference); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get inference ExternalModel %s/%s for IPP-owned route %s: %w", params.ModelNamespace, modelName, route.GetName(), err)
		}
		if inference.GetUID() != owner.UID {
			continue
		}
		log.Info("Deleting IPP-owned ExternalModel HTTPRoute while enabling Praxis", "name", route.GetName(), "namespace", route.GetNamespace(), "model", modelName)
		deleteOptions, err := validatedDeleteOptions(route)
		if err != nil {
			return fmt.Errorf("prepare IPP-owned HTTPRoute %s/%s deletion: %w", route.GetNamespace(), route.GetName(), err)
		}
		if err := c.Delete(ctx, route, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale IPP HTTPRoute %s/%s: %w", route.GetNamespace(), route.GetName(), err)
		}
	}
	return nil
}

func controllerOwner(obj *unstructured.Unstructured, gvk schema.GroupVersionKind, name string) *metav1.OwnerReference {
	var match *metav1.OwnerReference
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller && owner.APIVersion == gvk.GroupVersion().String() && owner.Kind == gvk.Kind && owner.Name == name {
			if match != nil {
				return nil
			}
			ownerRef := owner
			match = &ownerRef
		}
	}
	return match
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

	log.Info("Deleting IPP resource while enabling Praxis",
		"kind", ref.gvk.Kind, "name", ref.name, "namespace", ref.namespace)
	deleteOptions, err := validatedDeleteOptions(obj)
	if err != nil {
		return fmt.Errorf("prepare %s %s/%s deletion: %w", ref.gvk.Kind, ref.namespace, ref.name, err)
	}
	if ref.gvk == GVKDeployment {
		deleteOptions = append(deleteOptions, client.PropagationPolicy(metav1.DeletePropagationForeground))
	}
	if err := c.Delete(ctx, obj, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", ref.gvk.Kind, ref.namespace, ref.name, err)
	}
	return nil
}

func validatedDeleteOptions(obj client.Object) ([]client.DeleteOption, error) {
	uid := obj.GetUID()
	resourceVersion := obj.GetResourceVersion()
	if uid == "" || resourceVersion == "" {
		return nil, fmt.Errorf("validated object %s/%s is missing UID or resourceVersion", obj.GetNamespace(), obj.GetName())
	}
	return []client.DeleteOption{client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}}, nil
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
