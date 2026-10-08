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
	"fmt"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
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
	Baseline string // "default", "default+response-api-translation", "praxis", "absent", or ""
	Reason   string
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
//
// Baseline #3 (legacy metering) is intentionally omitted until a real customer
// example ConfigMap is collected.
func evaluatePluginsConfigMapMigration(ctx context.Context, c client.Client, params PlatformParams, tenant client.Object) (pluginsMigrationResult, error) {
	if forcePayloadProcessingMigration(tenant) {
		return pluginsMigrationResult{
			Decision: pluginsMigrationAllow,
			Baseline: "force",
			Reason:   "force-payload-processing-migration annotation allows migration of non-standard ConfigMap",
		}, nil
	}

	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{
		Namespace: params.GatewayNamespace,
		Name:      PayloadProcessingPluginsConfigMapForTenant(params.TenantIdentifier),
	}
	if err := c.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return pluginsMigrationResult{
				Decision: pluginsMigrationAllow,
				Baseline: "absent",
				Reason:   "plugins ConfigMap not present; nothing custom to preserve",
			}, nil
		}
		return pluginsMigrationResult{}, fmt.Errorf("get plugins ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
	}

	if isPraxisPluginsConfigMap(cm) {
		return pluginsMigrationResult{
			Decision: pluginsMigrationAllow,
			Baseline: "praxis",
			Reason:   "plugins ConfigMap already uses Praxis schema",
		}, nil
	}

	if !isIPPPluginsConfigMap(cm) {
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
			Reason:   "plugins ConfigMap matches known-good baseline " + baseline,
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
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
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
	return in
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
