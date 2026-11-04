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

package tenantreconcile

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	ippPreProcessingConfigKey = "custom-pre-processing-ipp-config.yaml"
	ippProcessingConfigKey    = "custom-ipp-config.yaml"

	// Praxis ConfigMap keys — presence means the object is already Praxis-shaped.
	praxisPreExtProcConfigKey = "pre-extproc.yaml"
	praxisExtProcConfigKey    = "extproc.yaml"
)

// pluginsMigrationDecision is the result of evaluating whether SkipIPP cleanup
// may delete/replace the live plugins ConfigMap.
type pluginsMigrationDecision int

const (
	pluginsMigrationAllow pluginsMigrationDecision = iota
	pluginsMigrationBlock
)

type pluginsMigrationResult struct {
	Decision pluginsMigrationDecision
	Baseline string // "default", "default+response-api-translation", "odh-3.5.0", "praxis", "absent", or ""
	Reason   string
	Forced   bool // an explicit force annotation accepts compatibility loss
}

type ippPluginRef struct {
	PluginRef string `json:"pluginRef" yaml:"pluginRef"`
}

type ippProfilePlugins struct {
	Request  []ippPluginRef `json:"request"  yaml:"request"`
	Response []ippPluginRef `json:"response" yaml:"response"`
}

type ippProfile struct {
	Name    string            `json:"name"    yaml:"name"`
	Plugins ippProfilePlugins `json:"plugins" yaml:"plugins"`
}

type ippPlugin struct {
	Type       string         `json:"type"                 yaml:"type"`
	Name       string         `json:"name,omitempty"       yaml:"name,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

type ippProcessorConfig struct {
	APIVersion string       `json:"apiVersion" yaml:"apiVersion"`
	Kind       string       `json:"kind"       yaml:"kind"`
	Plugins    []ippPlugin  `json:"plugins"    yaml:"plugins"`
	Profiles   []ippProfile `json:"profiles"   yaml:"profiles"`
}

// evaluatePluginsConfigMapMigration inspects the live plugins ConfigMap and
// decides whether automatic IPP→Praxis cleanup may proceed.
//
// Known-good baselines (RHOAIENG-98846):
//  1. Pure product default (response processors empty)
//  2. Default + response api-translation enabled
//  3. The ODH 3.5.0 default, an earlier shape of baseline 2
//
// A legacy metering baseline is intentionally omitted until a real customer
// example ConfigMap is collected.
func evaluatePluginsConfigMapMigration(ctx context.Context, reader client.Reader, params PlatformParams, tenant client.Object) (pluginsMigrationResult, error) {
	if mtc, ok := tenant.(*maasv1alpha1.MaasTenantConfig); ok {
		latest := &maasv1alpha1.MaasTenantConfig{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(mtc), latest); err != nil {
			return pluginsMigrationResult{}, fmt.Errorf("get MaasTenantConfig %s for migration override: %w", client.ObjectKeyFromObject(mtc), err)
		}
		tenant = latest
	}
	forced := forcePayloadProcessingMigration(tenant)

	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{
		Namespace: params.GatewayNamespace,
		Name:      PayloadProcessingPluginsConfigMapForTenant(params.TenantIdentifier),
	}
	if err := reader.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			result := pluginsMigrationResult{
				Decision: pluginsMigrationAllow,
				Baseline: "absent",
				Reason:   "plugins ConfigMap not present; nothing custom to preserve",
				Forced:   forced,
			}
			return result, nil
		}
		return pluginsMigrationResult{}, fmt.Errorf("get plugins ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
	}

	if isPraxisPluginsConfigMap(cm) {
		return pluginsMigrationResult{
			Decision: pluginsMigrationAllow,
			Baseline: "praxis",
			Reason:   "plugins ConfigMap already uses Praxis schema",
			Forced:   forced,
		}, nil
	}

	if !isIPPPluginsConfigMap(cm) {
		if forced {
			return pluginsMigrationResult{
				Decision: pluginsMigrationAllow,
				Baseline: "force",
				Reason:   forcedMigrationReason("force-payload-processing-migration annotation allows migration of an unrecognized ConfigMap", true),
				Forced:   true,
			}, nil
		}
		return pluginsMigrationResult{
			Decision: pluginsMigrationBlock,
			Reason: fmt.Sprintf(
				"plugins ConfigMap %s/%s has unrecognized data keys; automatic IPP→Praxis migration is not supported",
				cm.Namespace, cm.Name,
			),
		}, nil
	}

	baseline, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
	if ok {
		return pluginsMigrationResult{
			Decision: pluginsMigrationAllow,
			Baseline: baseline,
			Reason:   forcedMigrationReason("plugins ConfigMap matches known-good baseline "+baseline, forced),
			Forced:   forced,
		}, nil
	}
	if forced {
		return pluginsMigrationResult{
			Decision: pluginsMigrationAllow,
			Baseline: "force",
			Reason:   forcedMigrationReason("force-payload-processing-migration annotation allows migration of non-standard ConfigMap", true),
			Forced:   true,
		}, nil
	}

	return pluginsMigrationResult{
		Decision: pluginsMigrationBlock,
		Reason: fmt.Sprintf(
			"plugins ConfigMap %s/%s is non-standard (%s); automatic IPP→Praxis migration is blocked — review release notes and set %s=true on MaasTenantConfig to force migration",
			cm.Namespace, cm.Name, detail, AnnotationForcePayloadProcessingMigration,
		),
	}, nil
}

func forcedMigrationReason(reason string, forced bool) string {
	if !forced {
		return reason
	}
	return reason + "; forced migration accepts compatibility loss and does not establish IPP parity"
}

type apiTranslationMigrationIntent struct {
	Explicit bool
	Enabled  bool
}

func apiTranslationCompatibilityRisk(models []string) string {
	return fmt.Sprintf(
		"active ExternalModel provider bindings %s require legacy IPP request translation; the installed Praxis ExtProc cannot preserve OpenAI Chat Completions → provider-format dispatch yet",
		strings.Join(models, ", "),
	)
}

// persistAPITranslationMigrationRisk makes an accepted compatibility loss
// durable before cleanup removes the IPP ConfigMap. The marker is consumed on
// later reconciles to keep the tenant's Degraded condition informative while
// affected provider bindings remain active.
func persistAPITranslationMigrationRisk(ctx context.Context, c client.Client, tenant client.Object, risk string) error {
	if risk != APITranslationMigrationRiskExplicitFalse && risk != APITranslationMigrationRiskForced {
		return fmt.Errorf("unsupported API translation migration risk %q", risk)
	}
	return patchTenantAnnotations(ctx, c, tenant, func(annotations map[string]string) {
		annotations[AnnotationAPITranslationMigrationRisk] = risk
	})
}

func appendAcceptedAPITranslationMigrationRiskWarning(
	ctx context.Context,
	reader client.Reader,
	tenant client.Object,
	params *PlatformParams,
) error {
	annotations := tenant.GetAnnotations()
	risk := ""
	if annotations != nil {
		risk = annotations[AnnotationAPITranslationMigrationRisk]
	}
	if risk == "" {
		return nil
	}

	if risk == APITranslationMigrationRiskExplicitFalse {
		models, err := legacyIPPTranslationModels(ctx, reader, *params, tenant)
		if err != nil {
			return fmt.Errorf("recheck accepted API translation compatibility risk: %w", err)
		}
		if len(models) == 0 {
			return nil
		}
		params.Warnings = append(params.Warnings, apiTranslationCompatibilityRisk(models)+"; explicit spec.apiTranslation.enabled=false accepts this compatibility loss")
		return nil
	}

	warning := forcedMigrationReason("force-payload-processing-migration annotation allowed migration", true)
	models, err := legacyIPPTranslationModels(ctx, reader, *params, tenant)
	if err != nil {
		return fmt.Errorf("recheck forced API translation compatibility risk: %w", err)
	}
	if len(models) > 0 {
		warning = apiTranslationCompatibilityRisk(models) + "; " + forcedMigrationReason("force-payload-processing-migration annotation allowed migration", true)
	}
	params.Warnings = append(params.Warnings, warning)
	return nil
}

// ippBaselineTranslatesRequests reports whether a recognized IPP baseline ran
// model-provider-resolver and request-side api-translation, which sent each
// provider its targetModel and path. AGC applies both only with
// spec.apiTranslation.enabled; without it a migrated provider receives the
// MaaS model name on the default path, so a renamed model fails while it
// reports Ready.
func ippBaselineTranslatesRequests(baseline string) bool {
	switch baseline {
	case "default", "default+response-api-translation", "odh-3.5.0":
		return true
	default:
		return false
	}
}

// persistAPITranslationMigrationIntent records the translation of a recognized
// IPP baseline before cleanup can delete its ConfigMap. Update uses the live
// resourceVersion and retries conflicts so an operator's explicit true/false
// choice wins over automatic migration.
func persistAPITranslationMigrationIntent(ctx context.Context, c client.Client, reader client.Reader, tenant client.Object, persistDefault bool) (apiTranslationMigrationIntent, error) {
	mtc, ok := tenant.(*maasv1alpha1.MaasTenantConfig)
	if !ok {
		if persistDefault {
			return apiTranslationMigrationIntent{}, fmt.Errorf("cannot persist IPP api-translation intent on %T; the authoritative MaasTenantConfig is required", tenant)
		}
		return apiTranslationMigrationIntent{}, nil
	}

	key := client.ObjectKeyFromObject(mtc)
	intent := apiTranslationMigrationIntent{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &maasv1alpha1.MaasTenantConfig{}
		if err := reader.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("get MaasTenantConfig %s for api-translation migration intent: %w", key, err)
		}
		if latest.Spec.APITranslation != nil && latest.Spec.APITranslation.Enabled != nil {
			intent = apiTranslationMigrationIntent{Explicit: true, Enabled: *latest.Spec.APITranslation.Enabled}
			return nil
		}
		if !persistDefault {
			intent = apiTranslationMigrationIntent{}
			return nil
		}

		if latest.Spec.APITranslation == nil {
			latest.Spec.APITranslation = &maasv1alpha1.TenantAPITranslationConfig{}
		}
		enabled := true
		latest.Spec.APITranslation.Enabled = &enabled
		if err := c.Update(ctx, latest); err != nil {
			return err
		}
		intent = apiTranslationMigrationIntent{Enabled: true}
		return nil
	})
	if err != nil {
		return apiTranslationMigrationIntent{}, fmt.Errorf("persist api-translation migration intent on MaasTenantConfig %s: %w", key, err)
	}
	return intent, nil
}

// legacyIPPTranslationModels returns active ExternalModels whose provider
// binding is not the known-safe OpenAI-provider/OpenAI-Chat passthrough. The
// wire format alone is insufficient: Azure, Vertex, and Bedrock providers can
// use OpenAI-shaped requests while still needing provider-specific legacy
// path or response handling. Current Praxis ExtProc does not dispatch those
// legacy transforms after provider selection. The default tenant also covers
// the models it serves outside tenant namespaces (see tenantNamespaces).
func legacyIPPTranslationModels(ctx context.Context, reader client.Reader, params PlatformParams, tenant client.Object) ([]string, error) {
	namespace := params.ModelNamespace
	if namespace == "" {
		namespace = tenant.GetNamespace()
	}
	if namespace == "" {
		return nil, errors.New("model namespace is required to assess legacy IPP translation compatibility")
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "inference.opendatahub.io", Version: "v1alpha1", Kind: "ExternalModelList"})
	listOpts := []client.ListOption{client.InNamespace(namespace)}
	scope := "in " + namespace
	var tenantNS sets.Set[string]
	if params.TenantIdentifier == "" {
		var err error
		if tenantNS, err = tenantNamespaces(ctx, reader); err != nil {
			return nil, fmt.Errorf("assess legacy IPP translation compatibility: %w", err)
		}
		listOpts, scope = nil, "cluster-wide"
	}
	if err := reader.List(ctx, list, listOpts...); err != nil {
		return nil, fmt.Errorf("list ExternalModels %s for legacy IPP translation compatibility: %w", scope, err)
	}

	var affected []string
	for i := range list.Items {
		model := &list.Items[i]
		if !model.GetDeletionTimestamp().IsZero() || !servedByTenant(model, namespace, tenantNS, params) {
			continue
		}
		modelNamespace, label := model.GetNamespace(), model.GetName()
		if modelNamespace != namespace {
			label = modelNamespace + "/" + label
		}
		refs, found, err := unstructured.NestedSlice(model.Object, "spec", "externalProviderRefs")
		if err != nil {
			return nil, fmt.Errorf("read ExternalModel %s/%s provider refs: %w", modelNamespace, model.GetName(), err)
		}
		if !found {
			return nil, fmt.Errorf("ExternalModel %s/%s has no provider refs; cannot prove legacy IPP translation is unused", modelNamespace, model.GetName())
		}
		for index, raw := range refs {
			ref, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("ExternalModel %s/%s provider ref %d is malformed", modelNamespace, model.GetName(), index)
			}
			weight, err := externalProviderRefWeight(ref)
			if err != nil {
				return nil, fmt.Errorf("ExternalModel %s/%s provider ref %d: %w", modelNamespace, model.GetName(), index, err)
			}
			if weight <= 0 {
				continue
			}
			apiFormat, found, err := unstructured.NestedString(ref, "apiFormat")
			if err != nil {
				return nil, fmt.Errorf("read ExternalModel %s/%s provider ref %d apiFormat: %w", modelNamespace, model.GetName(), index, err)
			}
			if !found || strings.TrimSpace(apiFormat) == "" {
				return nil, fmt.Errorf("ExternalModel %s/%s provider ref %d has no apiFormat; cannot prove it is safe to migrate", modelNamespace, model.GetName(), index)
			}
			providerName, found, err := unstructured.NestedString(ref, "ref", "name")
			if err != nil || !found || strings.TrimSpace(providerName) == "" {
				affected = append(affected, fmt.Sprintf("%s (apiFormat=%s, provider unresolved)", label, apiFormat))
				continue
			}
			providerNamespace, found, err := unstructured.NestedString(ref, "ref", "namespace")
			if err != nil {
				return nil, fmt.Errorf("read ExternalModel %s/%s provider ref %d namespace: %w", modelNamespace, model.GetName(), index, err)
			}
			if !found || strings.TrimSpace(providerNamespace) == "" {
				providerNamespace = modelNamespace
			}
			if providerNamespace != modelNamespace {
				affected = append(affected, fmt.Sprintf("%s (apiFormat=%s, cross-namespace provider %s/%s)", label, apiFormat, providerNamespace, providerName))
				continue
			}

			provider := &unstructured.Unstructured{}
			provider.SetGroupVersionKind(schema.GroupVersionKind{Group: "inference.opendatahub.io", Version: "v1alpha1", Kind: "ExternalProvider"})
			if err := reader.Get(ctx, client.ObjectKey{Namespace: providerNamespace, Name: providerName}, provider); err != nil {
				if apierrors.IsNotFound(err) {
					affected = append(affected, fmt.Sprintf("%s (apiFormat=%s, provider %s unresolved)", label, apiFormat, providerName))
					continue
				}
				return nil, fmt.Errorf("read ExternalProvider %s/%s for ExternalModel %s/%s: %w", providerNamespace, providerName, modelNamespace, model.GetName(), err)
			}
			providerType, found, err := unstructured.NestedString(provider.Object, "spec", "provider")
			if err != nil || !found || strings.TrimSpace(providerType) == "" {
				affected = append(affected, fmt.Sprintf("%s (apiFormat=%s, provider %s has unknown type)", label, apiFormat, providerName))
				continue
			}
			if apiFormat != "openai-chat" || providerType != "openai" {
				affected = append(affected, fmt.Sprintf("%s (apiFormat=%s, provider=%s)", label, apiFormat, providerType))
			}
		}
	}
	sort.Strings(affected)
	return affected, nil
}

// servedByTenant reports whether the tenant serves the model: from its own
// namespace through its Gateway or, for the default tenant (non-nil
// tenantNS), as a model without gatewayRefs outside tenant namespaces. A model
// outside them that names a Gateway never used legacy IPP translation: IPP
// withdraws its route and skips it, and ai-gateway-controller attaches it only
// to a tenant that selects Praxis. Unlike ai-gateway-controller's implicit
// attachment, a model holding its home-tenant finalizer is still counted,
// which errs toward blocking the migration. The shared AppNamespace is out of
// scope, as for IPP route cleanup. Malformed placement fails closed.
func servedByTenant(model *unstructured.Unstructured, namespace string, tenantNS sets.Set[string], params PlatformParams) bool {
	if model.GetNamespace() == namespace {
		return externalModelTargetsGateway(model, params)
	}
	if model.GetNamespace() == params.AppNamespace {
		return false
	}
	refs, found, err := unstructured.NestedSlice(model.Object, "spec", "gatewayRefs")
	return tenantNS != nil && !tenantNS.Has(model.GetNamespace()) && (err != nil || !found || len(refs) == 0)
}

// tenantNamespaces returns the namespaces that hold a MaasTenantConfig or that
// an AITenant claims. The default tenant also serves the ExternalModels without
// gatewayRefs of every other namespace: ai-gateway-controller attaches them to
// its Gateway (RHOAIENG-101754), as its IPP served every namespace. Its
// IPP-to-Praxis migration therefore covers those models too. The CRD admits
// only the MaasTenantConfig named default-tenant, so any MaasTenantConfig
// marks a tenant namespace. Claims come from AITenant status.tenantNamespace,
// as ai-gateway-controller reads them, so both agree on which namespaces are
// unclaimed.
func tenantNamespaces(ctx context.Context, reader client.Reader) (sets.Set[string], error) {
	configs := &maasv1alpha1.MaasTenantConfigList{}
	if err := reader.List(ctx, configs); err != nil {
		return nil, fmt.Errorf("list MaasTenantConfigs: %w", err)
	}
	aitenants := &maasv1alpha1.AITenantList{}
	if err := reader.List(ctx, aitenants); err != nil {
		return nil, fmt.Errorf("list AITenants: %w", err)
	}
	namespaces := sets.New[string]()
	for i := range configs.Items {
		namespaces.Insert(configs.Items[i].Namespace)
	}
	for i := range aitenants.Items {
		if namespace := aitenants.Items[i].Status.TenantNamespace; namespace != "" {
			namespaces.Insert(namespace)
		}
	}
	return namespaces, nil
}

func externalProviderRefWeight(ref map[string]any) (int64, error) {
	value, found := ref["weight"]
	if !found || value == nil {
		return 1, nil
	}
	switch weight := value.(type) {
	case int:
		return int64(weight), nil
	case int32:
		return int64(weight), nil
	case int64:
		return weight, nil
	case float64:
		if weight == float64(int64(weight)) {
			return int64(weight), nil
		}
	}
	return 0, fmt.Errorf("weight has unsupported type %T", value)
}

func externalModelTargetsGateway(model *unstructured.Unstructured, params PlatformParams) bool {
	if params.GatewayName == "" || params.GatewayNamespace == "" {
		return true
	}
	refs, found, err := unstructured.NestedSlice(model.Object, "spec", "gatewayRefs")
	if err != nil || !found || len(refs) == 0 {
		return true
	}
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			return true // malformed placement must fail closed for compatibility
		}
		name, _ := ref["name"].(string)
		namespace, _ := ref["namespace"].(string)
		if name == params.GatewayName && namespace == params.GatewayNamespace {
			return true
		}
	}
	return false
}

func forcePayloadProcessingMigration(tenant client.Object) bool {
	if tenant == nil {
		return false
	}
	ann := tenant.GetAnnotations()
	return ann != nil && ann[AnnotationForcePayloadProcessingMigration] == "true"
}

func isPraxisPluginsConfigMap(cm *corev1.ConfigMap) bool {
	if cm == nil || cm.Data == nil {
		return false
	}
	_, hasIPPPre := cm.Data[ippPreProcessingConfigKey]
	_, hasIPPPost := cm.Data[ippProcessingConfigKey]
	if hasIPPPre || hasIPPPost {
		// Mixed IPP+Praxis keys must not skip the IPP baseline gate; cleanup
		// would delete customized IPP data without the force annotation.
		return false
	}
	// Permissive on purpose: ai-gateway-controller owns the Praxis ConfigMap
	// shape and may write pre-extproc.yaml / extproc.yaml independently. Any
	// Praxis key with no IPP keys means migration already happened.
	_, hasPre := cm.Data[praxisPreExtProcConfigKey]
	_, hasExt := cm.Data[praxisExtProcConfigKey]
	return hasPre || hasExt
}

func isIPPPluginsConfigMap(cm *corev1.ConfigMap) bool {
	if cm == nil || cm.Data == nil {
		return false
	}
	_, hasPre := cm.Data[ippPreProcessingConfigKey]
	_, hasPost := cm.Data[ippProcessingConfigKey]
	return hasPre && hasPost
}

func matchKnownGoodIPPPluginsConfigMap(cm *corev1.ConfigMap) (baseline string, ok bool, detail string) {
	pre, err := parseIPPProcessorConfig(cm.Data[ippPreProcessingConfigKey])
	if err != nil {
		return "", false, "pre-processing config: " + err.Error()
	}
	post, err := parseIPPProcessorConfig(cm.Data[ippProcessingConfigKey])
	if err != nil {
		return "", false, "processing config: " + err.Error()
	}

	if extra := unexpectedIPPConfigMapKeys(cm); len(extra) > 0 {
		return "", false, "unexpected data keys: " + strings.Join(extra, ", ")
	}

	if matchODH350PreProcessing(pre) && matchODH350Processing(post) {
		return "odh-3.5.0", true, ""
	}
	if !matchIPPPreProcessingDefault(pre) {
		return "", false, "pre-processing plugins/profiles deviate from product default"
	}

	responseBaseline, ok := matchIPPProcessingDefault(post)
	if !ok {
		return "", false, "processing plugins/profiles deviate from known-good baselines"
	}
	return responseBaseline, true, ""
}

func unexpectedIPPConfigMapKeys(cm *corev1.ConfigMap) []string {
	var extra []string
	for k := range cm.Data {
		if k == ippPreProcessingConfigKey || k == ippProcessingConfigKey {
			continue
		}
		extra = append(extra, k)
	}
	sort.Strings(extra)
	return extra
}

func parseIPPProcessorConfig(raw string) (ippProcessorConfig, error) {
	var cfg ippProcessorConfig
	if err := yaml.UnmarshalStrict([]byte(raw), &cfg); err != nil {
		return ippProcessorConfig{}, fmt.Errorf("parse yaml: %w", err)
	}
	if cfg.Kind != "" && cfg.Kind != "PayloadProcessorConfig" {
		return ippProcessorConfig{}, fmt.Errorf("unexpected kind %q", cfg.Kind)
	}
	return cfg, nil
}

func matchIPPPreProcessingDefault(cfg ippProcessorConfig) bool {
	wantPlugins := []ippPlugin{
		{
			Type: "body-field-to-header",
			Name: "model-extractor",
			Parameters: map[string]any{
				"fieldName":  "model",
				"headerName": "X-Gateway-Model-Name",
			},
		},
		{Type: "model-provider-resolver"},
	}
	wantRequest := []string{"model-extractor", "model-provider-resolver"}
	return matchPluginDefs(cfg.Plugins, wantPlugins) &&
		matchDefaultProfile(cfg.Profiles, wantRequest, nil)
}

// matchODH350PreProcessing matches the ODH 3.5.0 pre-processing chain, which
// had no model-provider-resolver yet. It is recognized only together with the
// ODH 3.5.0 processing chain.
func matchODH350PreProcessing(cfg ippProcessorConfig) bool {
	wantPlugins := []ippPlugin{{
		Type: "body-field-to-header",
		Name: "model-extractor",
		Parameters: map[string]any{
			"fieldName":  "model",
			"headerName": "X-Gateway-Model-Name",
		},
	}}
	return matchPluginDefs(cfg.Plugins, wantPlugins) &&
		matchDefaultProfile(cfg.Profiles, []string{"model-extractor"}, nil)
}

// matchODH350Processing matches the ODH 3.5.0 processing chain: the product
// default without maas-headers-guard, with response api-translation.
func matchODH350Processing(cfg ippProcessorConfig) bool {
	wantPlugins := []ippPlugin{
		{Type: "stream-usage-enforcer"},
		{Type: "model-provider-resolver"},
		{Type: "api-translation"},
		{Type: "apikey-injection"},
	}
	wantRequest := []string{"stream-usage-enforcer", "model-provider-resolver", "api-translation", "apikey-injection"}
	return matchPluginDefs(cfg.Plugins, wantPlugins) &&
		matchDefaultProfile(cfg.Profiles, wantRequest, []string{"api-translation"})
}

// matchIPPProcessingDefault returns the baseline name when cfg matches a
// known-good processing chain. Allowed response chains: empty, or
// [api-translation] only.
func matchIPPProcessingDefault(cfg ippProcessorConfig) (baseline string, ok bool) {
	wantPlugins := []ippPlugin{
		{Type: "maas-headers-guard", Name: "maas-headers-guard"},
		{Type: "stream-usage-enforcer"},
		{Type: "model-provider-resolver"},
		{Type: "api-translation"},
		{Type: "apikey-injection"},
	}
	wantRequest := []string{
		"maas-headers-guard",
		"stream-usage-enforcer",
		"model-provider-resolver",
		"api-translation",
		"apikey-injection",
	}
	if !matchPluginDefs(cfg.Plugins, wantPlugins) {
		return "", false
	}

	switch {
	case matchDefaultProfile(cfg.Profiles, wantRequest, nil):
		return "default", true
	case matchDefaultProfile(cfg.Profiles, wantRequest, []string{"api-translation"}):
		return "default+response-api-translation", true
	default:
		return "", false
	}
}

// matchPluginDefs compares plugin definitions in order. IPP chains are
// order-sensitive (processing order), so a reordered-but-equal set is treated
// as non-standard and blocks automatic migration.
func matchPluginDefs(got, want []ippPlugin) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			return false
		}
		// Name is optional in shipped YAML for some plugins; when present on
		// either side it must match. Empty name is treated as equal to type for
		// the few plugins that omit it.
		gotName := got[i].Name
		if gotName == "" {
			gotName = got[i].Type
		}
		wantName := want[i].Name
		if wantName == "" {
			wantName = want[i].Type
		}
		if gotName != wantName {
			return false
		}
		if !reflect.DeepEqual(normalizeParams(got[i].Parameters), normalizeParams(want[i].Parameters)) {
			return false
		}
	}
	return true
}

func normalizeParams(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeParamValue(v)
	}
	return out
}

// normalizeParamValue coerces YAML/JSON number types so Go int literals in
// baselines compare equal to float64 values produced by yaml.Unmarshal.
func normalizeParamValue(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case float32:
		return float64(n)
	case []any:
		out := make([]any, len(n))
		for i := range n {
			out[i] = normalizeParamValue(n[i])
		}
		return out
	case map[string]any:
		return normalizeParams(n)
	default:
		return v
	}
}

func matchDefaultProfile(profiles []ippProfile, wantRequest, wantResponse []string) bool {
	if len(profiles) != 1 || profiles[0].Name != "default" {
		return false
	}
	return matchRefs(profiles[0].Plugins.Request, wantRequest) &&
		matchRefs(profiles[0].Plugins.Response, wantResponse)
}

func matchRefs(got []ippPluginRef, want []string) bool {
	if len(want) == 0 {
		return len(got) == 0
	}
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].PluginRef != want[i] {
			return false
		}
	}
	return true
}

func setPayloadProcessingMigrationAnnotation(ctx context.Context, c client.Client, tenant client.Object, value string) error {
	return patchTenantAnnotations(ctx, c, tenant, func(annotations map[string]string) {
		if value == "" {
			delete(annotations, AnnotationPayloadProcessingMigration)
			return
		}
		annotations[AnnotationPayloadProcessingMigration] = value
	})
}
