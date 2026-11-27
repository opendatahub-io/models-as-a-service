package tenantreconcile

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// ippFilterName is the ext_proc filter that runs the payload processor after auth.
const ippFilterName = "envoy.filters.http.ext_proc.ipp"

// HasIPPProcessingConfig reports whether cm carries the config the payload-processing
// Deployment loads. ai-gateway-controller writes praxis ConfigMaps under the same names
// without it.
func HasIPPProcessingConfig(cm *corev1.ConfigMap) bool {
	_, ok := cm.Data[ippProcessingConfigKey]
	return ok
}

// ippResponseProcessingNeeded reports whether the IPP config in raw needs ext_proc.ipp on
// the response path, and why. Only a config that parses strictly, has a profile and lists
// no response plugin in any profile answers false. Fields the parser does not model fail
// the parse and keep response processing on; postProcessing is one, and the IPP runs it on
// every response.
func ippResponseProcessingNeeded(raw string) (bool, string) {
	cfg, err := parseIPPProcessorConfig(raw)
	if err != nil {
		return true, fmt.Sprintf("cannot read the IPP config: %v", err)
	}
	if len(cfg.Profiles) == 0 {
		return true, "the IPP config has no profiles"
	}
	for _, profile := range cfg.Profiles {
		if len(profile.Plugins.Response) > 0 {
			return true, fmt.Sprintf("profile %q has response plugins", profile.Name)
		}
	}
	return false, "no profile has response plugins"
}

// resolveIPPResponseOff reports whether ext_proc.ipp leaves the response path for the
// tenant, and why. The answer comes from the plugins ConfigMap the IPP loads: the live one
// when ApplyRendered leaves it as it is, or when liveOnly is set for tenants whose IPP
// objects are not applied at all; otherwise the rendered one this reconcile applies. A read
// error is returned rather than read as "on", which would move a tenant back to the failing
// modes and rebuild its gateway listeners over a transient error.
func resolveIPPResponseOff(
	ctx context.Context,
	reader client.Reader,
	rendered []unstructured.Unstructured,
	params PlatformParams,
	configUID types.UID,
	liveOnly bool,
) (bool, string, error) {
	key := types.NamespacedName{
		Namespace: params.GatewayNamespace,
		Name:      PayloadProcessingPluginsConfigMapForTenant(params.TenantIdentifier),
	}
	live := &corev1.ConfigMap{}
	err := reader.Get(ctx, key, live)
	switch {
	case err == nil && (liveOnly || isUnmanaged(live) || isControlledByOther(live, configUID)):
		raw, ok := live.Data[ippProcessingConfigKey]
		if !ok {
			return false, fmt.Sprintf("ConfigMap %s has no %s", key, ippProcessingConfigKey), nil
		}
		needed, reason := ippResponseProcessingNeeded(raw)
		return !needed, fmt.Sprintf("ConfigMap %s: %s", key, reason), nil
	case err != nil && !apierrors.IsNotFound(err):
		return false, "", fmt.Errorf("get ConfigMap %s: %w", key, err)
	case liveOnly:
		return false, fmt.Sprintf("ConfigMap %s not found", key), nil
	}

	raw, found, err := renderedIPPProcessingConfig(rendered)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, fmt.Sprintf("rendered ConfigMap %s has no %s", PayloadProcessingPluginsConfigMapName, ippProcessingConfigKey), nil
	}
	needed, reason := ippResponseProcessingNeeded(raw)
	return !needed, "rendered ConfigMap: " + reason, nil
}

// configUID returns the UID ApplyRendered owns objects with; empty without a Config.
func configUID(mcfg *maasv1alpha1.Config) types.UID {
	if mcfg == nil {
		return ""
	}
	return mcfg.UID
}

// renderedIPPProcessingConfig returns the IPP config from the rendered plugins ConfigMap,
// which still has its base name: PostRender renames it per tenant.
func renderedIPPProcessingConfig(rendered []unstructured.Unstructured) (string, bool, error) {
	for i := range rendered {
		r := &rendered[i]
		if r.GroupVersionKind() != GVKConfigMap || r.GetName() != PayloadProcessingPluginsConfigMapName {
			continue
		}
		raw, found, err := unstructured.NestedString(r.Object, "data", ippProcessingConfigKey)
		if err != nil {
			return "", false, fmt.Errorf("read rendered ConfigMap %s: %w", PayloadProcessingPluginsConfigMapName, err)
		}
		return raw, found, nil
	}
	return "", false, nil
}

// disableIPPResponseProcessing sets response SKIP/NONE/SKIP on every patch that inserts
// ext_proc.ipp. The three modes change together: the IPP answers response headers only with
// its first body chunk, so header SEND with body NONE hangs every request, and header SKIP
// with a streamed body turns that answer into one Envoy rejects.
func disableIPPResponseProcessing(configPatches []any) error {
	disabled := 0
	for i := range configPatches {
		patch, ok := configPatches[i].(map[string]any)
		if !ok {
			return fmt.Errorf("EnvoyFilter configPatches[%d] is not an object", i)
		}
		if name, _, _ := unstructured.NestedString(patch, "patch", "value", "name"); name != ippFilterName {
			continue
		}
		modePath := []string{"patch", "value", "typed_config", "processing_mode"}
		mode, _, err := unstructured.NestedMap(patch, modePath...)
		if err != nil {
			return fmt.Errorf("read configPatches[%d] processing_mode: %w", i, err)
		}
		if mode == nil {
			mode = map[string]any{}
		}
		mode["response_header_mode"] = "SKIP"
		mode["response_body_mode"] = "NONE"
		mode["response_trailer_mode"] = "SKIP"
		if err := unstructured.SetNestedMap(patch, mode, modePath...); err != nil {
			return fmt.Errorf("write configPatches[%d] processing_mode: %w", i, err)
		}
		disabled++
	}
	if disabled == 0 {
		return errors.New("EnvoyFilter configPatches: no " + ippFilterName + " insert")
	}
	return nil
}
