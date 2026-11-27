package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const externalMeteringStreamingProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: external-metering-streaming
profiles:
- name: default
  plugins:
    request: []
    response:
    - pluginRef: external-metering-streaming
`

const secondProfileResponseProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: model-provider-resolver
- type: api-translation
profiles:
- name: default
  plugins:
    request:
    - pluginRef: model-provider-resolver
    response: []
- name: anthropic
  plugins:
    request:
    - pluginRef: api-translation
    response:
    - pluginRef: api-translation
`

const postProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: model-provider-resolver
- type: external-metering
profiles:
- name: default
  plugins:
    request:
    - pluginRef: model-provider-resolver
    response: []
postProcessing:
  plugins:
  - pluginRef: external-metering
`

const profilePickerYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: model-provider-resolver
profilePicker:
  pluginRef: single-profile-picker
profiles:
- name: default
  plugins:
    request:
    - pluginRef: model-provider-resolver
    response: []
`

func TestIPPResponseProcessingNeeded(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "product default", raw: defaultProcessingYAML, want: false},
		{name: "response api-translation", raw: responseAPITranslationProcessingYAML, want: true},
		{name: "response chunk processor", raw: externalMeteringStreamingProcessingYAML, want: true},
		{name: "one of two profiles has a response plugin", raw: secondProfileResponseProcessingYAML, want: true},
		{name: "postProcessing runs on every response", raw: postProcessingYAML, want: true},
		{name: "field the parser does not model", raw: profilePickerYAML, want: true},
		{name: "invalid YAML", raw: "profiles: [", want: true},
		{name: "no profiles", raw: "apiVersion: llm-d.ai/v1alpha1\nkind: PayloadProcessorConfig\n", want: true},
		{name: "empty", raw: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := ippResponseProcessingNeeded(tt.raw)
			assert.Equal(t, tt.want, got, reason)
			assert.NotEmpty(t, reason)
		})
	}
}

func TestIPPResponseProcessingNeededForRenderedDefault(t *testing.T) {
	raw, found, err := renderedIPPProcessingConfig(renderOverlayResources(t, "tenant-ns"))
	require.NoError(t, err)
	require.True(t, found, "rendered plugins ConfigMap has no %s", ippProcessingConfigKey)

	needed, reason := ippResponseProcessingNeeded(raw)
	assert.False(t, needed, "the shipped profile should take ipp off the response path: %s", reason)
}

func TestResolveIPPResponseOff(t *testing.T) {
	const (
		gatewayNS = "openshift-ingress"
		tenantID  = "team-a"
		cfgUID    = types.UID("cfg-uid")
	)
	params := PlatformParams{GatewayNamespace: gatewayNS, TenantIdentifier: tenantID}
	rendered := []unstructured.Unstructured{*renderedPluginsConfigMap(defaultProcessingYAML)}

	liveConfigMap := func(processing string, annotations map[string]string, owners ...metav1.OwnerReference) *corev1.ConfigMap {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            PayloadProcessingPluginsConfigMapForTenant(tenantID),
				Namespace:       gatewayNS,
				Annotations:     annotations,
				OwnerReferences: owners,
			},
			Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML},
		}
		if processing != "" {
			cm.Data[ippProcessingConfigKey] = processing
		}
		return cm
	}
	unmanaged := map[string]string{AnnotationManaged: "false"}

	tests := []struct {
		name     string
		live     *corev1.ConfigMap
		rendered []unstructured.Unstructured
		liveOnly bool
		wantOff  bool
	}{
		{
			name:     "unmanaged live default",
			live:     liveConfigMap(defaultProcessingYAML, unmanaged),
			rendered: rendered,
			wantOff:  true,
		},
		{
			name:     "unmanaged live response plugin wins over the rendered default",
			live:     liveConfigMap(responseAPITranslationProcessingYAML, unmanaged),
			rendered: rendered,
			wantOff:  false,
		},
		{
			name: "live controlled by another owner",
			live: liveConfigMap(responseAPITranslationProcessingYAML, nil, metav1.OwnerReference{
				APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid", Controller: ptr.To(true),
			}),
			rendered: rendered,
			wantOff:  false,
		},
		{
			name:     "managed live is replaced by the rendered default",
			live:     liveConfigMap(responseAPITranslationProcessingYAML, map[string]string{AnnotationManaged: "true"}),
			rendered: rendered,
			wantOff:  true,
		},
		{
			name:     "no live ConfigMap uses the rendered default",
			rendered: rendered,
			wantOff:  true,
		},
		{
			name:     "unmanaged live without the IPP config",
			live:     liveConfigMap("", unmanaged),
			rendered: rendered,
			wantOff:  false,
		},
		{
			name:    "nothing rendered",
			wantOff: false,
		},
		{
			name:     "live only reads a managed live ConfigMap",
			live:     liveConfigMap(defaultProcessingYAML, nil),
			liveOnly: true,
			wantOff:  true,
		},
		{
			name:     "live only without a live ConfigMap",
			rendered: rendered,
			liveOnly: true,
			wantOff:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder()
			if tt.live != nil {
				builder = builder.WithObjects(tt.live)
			}
			off, reason, err := resolveIPPResponseOff(t.Context(), builder.Build(), tt.rendered, params, cfgUID, tt.liveOnly)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOff, off, reason)
		})
	}

	t.Run("read error", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("connection refused")
			},
		}).Build()
		_, _, err := resolveIPPResponseOff(t.Context(), reader, rendered, params, cfgUID, false)
		require.Error(t, err)
	})
}

func TestPatchPayloadProcessingEnvoyFilterIPPResponseModes(t *testing.T) {
	ippOn := map[string]any{
		"request_header_mode":   "SEND",
		"response_header_mode":  "SEND",
		"request_body_mode":     "FULL_DUPLEX_STREAMED",
		"response_body_mode":    "FULL_DUPLEX_STREAMED",
		"request_trailer_mode":  "SEND",
		"response_trailer_mode": "SEND",
	}
	ippOff := map[string]any{
		"request_header_mode":   "SEND",
		"response_header_mode":  "SKIP",
		"request_body_mode":     "FULL_DUPLEX_STREAMED",
		"response_body_mode":    "NONE",
		"request_trailer_mode":  "SEND",
		"response_trailer_mode": "SKIP",
	}
	ippPre := map[string]any{
		"request_header_mode":  "SEND",
		"response_header_mode": "SKIP",
		"request_body_mode":    "FULL_DUPLEX_STREAMED",
		"response_body_mode":   "NONE",
		"request_trailer_mode": "SEND",
	}

	tests := []struct {
		name           string
		routerFallback bool
		responseOff    bool
		wantIPPInserts int
		wantIPPMode    map[string]any
	}{
		{name: "wasm anchors, response on", wantIPPInserts: 2, wantIPPMode: ippOn},
		{name: "wasm anchors, response off", responseOff: true, wantIPPInserts: 2, wantIPPMode: ippOff},
		{name: "router fallback, response on", routerFallback: true, wantIPPInserts: 1, wantIPPMode: ippOn},
		{name: "router fallback, response off", routerFallback: true, responseOff: true, wantIPPInserts: 1, wantIPPMode: ippOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ef := requireResource(t, renderOverlayResources(t, "tenant-ns"), GVKEnvoyFilter, PayloadProcessingEnvoyFilterName(""))
			params := PlatformParams{
				GatewayNamespace:                       "gateway-ns",
				GatewayName:                            "custom-gateway",
				AppNamespace:                           "tenant-ns",
				PayloadProcessingRouterExtProcFallback: tt.routerFallback,
				IPPResponseOff:                         tt.responseOff,
			}
			require.NoError(t, patchPayloadProcessingEnvoyFilter(logr.Discard(), ef, params))

			modes := extProcModesByFilter(t, ef)
			require.Len(t, modes[ippFilterName], tt.wantIPPInserts)
			for _, mode := range modes[ippFilterName] {
				assert.Equal(t, tt.wantIPPMode, mode)
			}
			require.Len(t, modes["envoy.filters.http.ext_proc.ipp-pre"], tt.wantIPPInserts)
			for _, mode := range modes["envoy.filters.http.ext_proc.ipp-pre"] {
				assert.Equal(t, ippPre, mode)
			}
		})
	}
}

func TestDisableIPPResponseProcessingRequiresAnIPPInsert(t *testing.T) {
	patches := []any{map[string]any{"patch": map[string]any{"value": map[string]any{"name": "envoy.filters.http.ext_proc.ipp-pre"}}}}
	require.Error(t, disableIPPResponseProcessing(patches))
}

func TestRunPlatformRendersIPPResponseModesFromPluginsConfigMap(t *testing.T) {
	const (
		tenantName = "existing-team"
		appNs      = "ai-tenant-existing-team"
		gwNS       = "openshift-ingress"
		gwName     = "existing-gateway"
	)
	tests := []struct {
		name         string
		live         *corev1.ConfigMap
		wantHeader   string
		wantBody     string
		wantTrailers string
	}{
		{
			name:         "first install renders the product default",
			wantHeader:   "SKIP",
			wantBody:     "NONE",
			wantTrailers: "SKIP",
		},
		{
			name: "edited profile with a response plugin",
			live: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:        PayloadProcessingPluginsConfigMapForTenant(tenantName),
					Namespace:   gwNS,
					Annotations: map[string]string{AnnotationManaged: "false"},
				},
				Data: map[string]string{
					ippPreProcessingConfigKey: defaultPreProcessingYAML,
					ippProcessingConfigKey:    responseAPITranslationProcessingYAML,
				},
			},
			wantHeader:   "SEND",
			wantBody:     "FULL_DUPLEX_STREAMED",
			wantTrailers: "SEND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := praxisTestScheme(t)
			tenant := praxisTenantConfig(appNs, tenantName)
			tenant.Annotations[AnnotationPayloadProcessingStatus] = PayloadProcessingStatusCleanupComplete
			mcfg := &maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			}
			gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS}}
			seed := []client.Object{mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName)}
			if tt.live != nil {
				seed = append(seed, tt.live)
			}
			cl := runPlatformTestClient(t, scheme, seed, nil)

			_, err := RunPlatform(t.Context(), logr.Discard(), cl, scheme, tenant,
				PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName}, Source: "aitenant"},
				platformOverlayManifestPath(t), appNs, "controller-ns", "https://kubernetes.default.svc", "opendatahub", mcfg)
			require.NoError(t, err)

			ef := &unstructured.Unstructured{}
			ef.SetGroupVersionKind(GVKEnvoyFilter)
			require.NoError(t, cl.Get(t.Context(), types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingEnvoyFilterName(tenantName)}, ef))
			modes := extProcModesByFilter(t, ef)[ippFilterName]
			require.NotEmpty(t, modes)
			for _, mode := range modes {
				assert.Equal(t, tt.wantHeader, mode["response_header_mode"])
				assert.Equal(t, tt.wantBody, mode["response_body_mode"])
				assert.Equal(t, tt.wantTrailers, mode["response_trailer_mode"])
			}
		})
	}
}

// extProcModesByFilter returns the processing_mode of every ext_proc insert in the
// EnvoyFilter, keyed by filter name.
func extProcModesByFilter(t *testing.T, ef *unstructured.Unstructured) map[string][]map[string]any {
	t.Helper()
	configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	modes := map[string][]map[string]any{}
	for _, p := range configPatches {
		patch, ok := p.(map[string]any)
		require.True(t, ok)
		name, _, _ := unstructured.NestedString(patch, "patch", "value", "name")
		mode, found, err := unstructured.NestedMap(patch, "patch", "value", "typed_config", "processing_mode")
		require.NoError(t, err)
		if name == "" || !found {
			continue
		}
		modes[name] = append(modes[name], mode)
	}
	return modes
}

func renderedPluginsConfigMap(processing string) *unstructured.Unstructured {
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(GVKConfigMap)
	cm.SetName(PayloadProcessingPluginsConfigMapName)
	cm.SetNamespace("openshift-ingress")
	_ = unstructured.SetNestedStringMap(cm.Object, map[string]string{
		ippPreProcessingConfigKey: defaultPreProcessingYAML,
		ippProcessingConfigKey:    processing,
	}, "data")
	return cm
}

const customRequestOnlyProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: custom-only
profiles:
- name: default
  plugins:
    request:
    - pluginRef: custom-only
    response: []
`

const customResponseProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: custom-only
profiles:
- name: default
  plugins:
    request:
    - pluginRef: custom-only
    response:
    - pluginRef: custom-only
`

func TestRunPlatformRendersEnvoyFilterWhileMigrationBlocked(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	tests := []struct {
		name         string
		processing   string
		wantHeader   string
		wantBody     string
		wantTrailers string
	}{
		{
			name:         "customized profile without response plugins",
			processing:   customRequestOnlyProcessingYAML,
			wantHeader:   "SKIP",
			wantBody:     "NONE",
			wantTrailers: "SKIP",
		},
		{
			name:         "customized profile with a response plugin",
			processing:   customResponseProcessingYAML,
			wantHeader:   "SEND",
			wantBody:     "FULL_DUPLEX_STREAMED",
			wantTrailers: "SEND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := praxisTestScheme(t)
			tenant := praxisTenantConfig(appNs, tenantName)
			mcfg := &maasv1alpha1.Config{
				ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
			}
			gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS}}
			ippDeployment := unstructuredIPPObject(GVKDeployment, gwNS, PayloadProcessingDeploymentName(tenantName), nil)
			setConfigControllerOwnerRef(ippDeployment, mcfg.UID)
			pluginsCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: PayloadProcessingPluginsConfigMapForTenant(tenantName), Namespace: gwNS},
				Data: map[string]string{
					ippPreProcessingConfigKey: defaultPreProcessingYAML,
					ippProcessingConfigKey:    tt.processing,
				},
			}
			var applied []appliedResource
			cl := runPlatformTestClient(t, scheme, []client.Object{
				mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), ippDeployment, pluginsCM,
			}, &applied)

			result, err := RunPlatform(t.Context(), logr.Discard(), cl, scheme, tenant,
				PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName}, SkipIPP: true, Source: "aitenant"},
				platformOverlayManifestPath(t), appNs, "controller-ns", "https://kubernetes.default.svc", "opendatahub", mcfg)
			require.NoError(t, err)
			require.True(t, result.DeploymentPending)
			require.Contains(t, result.Detail, AnnotationForcePayloadProcessingMigration, "migration should be blocked")

			legacyNames := map[string]bool{
				PayloadProcessingDeploymentName(tenantName):            true,
				PayloadPreProcessingDeploymentName(tenantName):         true,
				PayloadProcessingServiceName(tenantName):               true,
				PayloadPreProcessingServiceName(tenantName):            true,
				PayloadProcessingServiceAccountName(tenantName):        true,
				PayloadProcessingPluginsConfigMapForTenant(tenantName): true,
				PayloadProcessingNetworkPolicyName(tenantName):         true,
				PayloadProcessingEnvoyFilterName(tenantName):           true,
			}
			var appliedLegacy []string
			for _, res := range applied {
				if res.namespace == gwNS && legacyNames[res.name] {
					appliedLegacy = append(appliedLegacy, res.gvk.Kind+"/"+res.name)
				}
			}
			assert.Equal(t, []string{"EnvoyFilter/" + PayloadProcessingEnvoyFilterName(tenantName)}, appliedLegacy,
				"only the EnvoyFilter of the legacy IPP objects is applied while blocked")

			ef := &unstructured.Unstructured{}
			ef.SetGroupVersionKind(GVKEnvoyFilter)
			require.NoError(t, cl.Get(t.Context(), types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingEnvoyFilterName(tenantName)}, ef))
			modes := extProcModesByFilter(t, ef)[ippFilterName]
			require.NotEmpty(t, modes)
			for _, mode := range modes {
				assert.Equal(t, tt.wantHeader, mode["response_header_mode"])
				assert.Equal(t, tt.wantBody, mode["response_body_mode"])
				assert.Equal(t, tt.wantTrailers, mode["response_trailer_mode"])
			}
		})
	}
}

func TestRunPlatformSteadyPraxisTenantLeavesIPPEnvoyFilter(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	tenant.Annotations[AnnotationPayloadProcessingStatus] = PayloadProcessingStatusCleanupComplete
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS}}
	pluginsName := PayloadProcessingPluginsConfigMapForTenant(tenantName)
	efName := PayloadProcessingEnvoyFilterName(tenantName)
	pluginsReads, efApplies := 0, 0
	cl := newSSAFakeClientBuilder(scheme).
		WithObjects(mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Namespace == gwNS && key.Name == pluginsName {
					pluginsReads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if u, ok := obj.(interface{ GetName() string }); ok && u.GetName() == efName {
					efApplies++
				}
				return c.Apply(ctx, obj, opts...)
			},
		}).Build()

	_, err := RunPlatform(t.Context(), logr.Discard(), cl, scheme, tenant,
		PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName}, SkipIPP: true, Source: "aitenant"},
		platformOverlayManifestPath(t), appNs, "controller-ns", "https://kubernetes.default.svc", "opendatahub", mcfg)
	require.NoError(t, err)
	assert.Zero(t, efApplies, "ai-gateway-controller owns the EnvoyFilter of a steady praxis tenant")
	assert.Zero(t, pluginsReads, "a steady praxis tenant has no IPP plugins ConfigMap to read")
}
