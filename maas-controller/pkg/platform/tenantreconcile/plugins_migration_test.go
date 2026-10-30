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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const defaultPreProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: body-field-to-header
  name: model-extractor
  parameters:
    fieldName: model
    headerName: X-Gateway-Model-Name
- type: model-provider-resolver
profiles:
- name: default
  plugins:
    request:
    - pluginRef: model-extractor
    - pluginRef: model-provider-resolver
`

const defaultProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: maas-headers-guard
  name: maas-headers-guard
- type: stream-usage-enforcer
- type: model-provider-resolver
- type: api-translation
- type: apikey-injection
profiles:
- name: default
  plugins:
    request:
    - pluginRef: maas-headers-guard
    - pluginRef: stream-usage-enforcer
    - pluginRef: model-provider-resolver
    - pluginRef: api-translation
    - pluginRef: apikey-injection
    response: []
`

const responseAPITranslationProcessingYAML = `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: maas-headers-guard
  name: maas-headers-guard
- type: stream-usage-enforcer
- type: model-provider-resolver
- type: api-translation
- type: apikey-injection
profiles:
- name: default
  plugins:
    request:
    - pluginRef: maas-headers-guard
    - pluginRef: stream-usage-enforcer
    - pluginRef: model-provider-resolver
    - pluginRef: api-translation
    - pluginRef: apikey-injection
    response:
    - pluginRef: api-translation
`

func pluginsMigrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
	return scheme
}

func TestMatchKnownGoodIPPPluginsConfigMap(t *testing.T) {
	t.Parallel()

	t.Run("pure default", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    defaultProcessingYAML,
		}}
		baseline, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		require.True(t, ok, detail)
		assert.Equal(t, "default", baseline)
	})

	t.Run("default plus response api-translation", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    responseAPITranslationProcessingYAML,
		}}
		baseline, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		require.True(t, ok, detail)
		assert.Equal(t, "default+response-api-translation", baseline)
	})

	t.Run("custom plugin blocks", func(t *testing.T) {
		t.Parallel()
		custom := `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: maas-headers-guard
  name: maas-headers-guard
- type: stream-usage-enforcer
- type: model-provider-resolver
- type: api-translation
- type: apikey-injection
- type: custom-metering
profiles:
- name: default
  plugins:
    request:
    - pluginRef: maas-headers-guard
    - pluginRef: stream-usage-enforcer
    - pluginRef: model-provider-resolver
    - pluginRef: api-translation
    - pluginRef: apikey-injection
    - pluginRef: custom-metering
    response: []
`
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    custom,
		}}
		_, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		assert.False(t, ok)
		assert.Contains(t, detail, "processing plugins/profiles")
	})

	t.Run("extra data key blocks", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    defaultProcessingYAML,
			"extra.yaml":              "nope",
		}}
		_, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		assert.False(t, ok)
		assert.Contains(t, detail, "unexpected data keys")
	})

	t.Run("unknown yaml field blocks", func(t *testing.T) {
		t.Parallel()
		withUnknown := defaultProcessingYAML + "postProcessing:\n  enabled: true\n"
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    withUnknown,
		}}
		_, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		assert.False(t, ok)
		assert.Contains(t, detail, "parse yaml")
	})

	t.Run("empty yaml values block", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: "",
			ippProcessingConfigKey:    "",
		}}
		_, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		assert.False(t, ok)
		// Empty YAML unmarshals to a zero-value config (nil plugins/profiles),
		// which fails the product-default match rather than panicking.
		assert.Contains(t, detail, "pre-processing plugins/profiles")
	})

	t.Run("reordered plugins block", func(t *testing.T) {
		t.Parallel()
		// Same plugin set as the product default, but request chain order differs.
		reordered := `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: maas-headers-guard
  name: maas-headers-guard
- type: stream-usage-enforcer
- type: model-provider-resolver
- type: api-translation
- type: apikey-injection
profiles:
- name: default
  plugins:
    request:
    - pluginRef: stream-usage-enforcer
    - pluginRef: maas-headers-guard
    - pluginRef: model-provider-resolver
    - pluginRef: api-translation
    - pluginRef: apikey-injection
    response: []
`
		cm := &corev1.ConfigMap{Data: map[string]string{
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    reordered,
		}}
		_, ok, detail := matchKnownGoodIPPPluginsConfigMap(cm)
		assert.False(t, ok)
		assert.Contains(t, detail, "processing plugins/profiles")
	})
}

func TestNormalizeParamsNumericEquivalence(t *testing.T) {
	t.Parallel()
	// YAML unmarshaling typically yields float64 for numbers; baselines may use int.
	got := normalizeParams(map[string]any{"timeoutMs": float64(30), "nested": map[string]any{"retries": float64(3)}})
	want := normalizeParams(map[string]any{"timeoutMs": 30, "nested": map[string]any{"retries": 3}})
	assert.Equal(t, want, got)
}

func TestIsPraxisPluginsConfigMap(t *testing.T) {
	t.Parallel()

	t.Run("praxis keys only", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			praxisPreExtProcConfigKey: "server: {}",
			praxisExtProcConfigKey:    "server: {}",
		}}
		assert.True(t, isPraxisPluginsConfigMap(cm))
	})

	t.Run("partial praxis key is already praxis", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			praxisExtProcConfigKey: "server: {}",
		}}
		assert.True(t, isPraxisPluginsConfigMap(cm))
	})

	t.Run("mixed ipp and praxis keys rejected", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{Data: map[string]string{
			praxisPreExtProcConfigKey: "server: {}",
			praxisExtProcConfigKey:    "server: {}",
			ippPreProcessingConfigKey: defaultPreProcessingYAML,
			ippProcessingConfigKey:    defaultProcessingYAML,
		}}
		assert.False(t, isPraxisPluginsConfigMap(cm))
	})
}

func TestEvaluatePluginsConfigMapMigration(t *testing.T) {
	t.Parallel()
	scheme := pluginsMigrationScheme(t)
	// Empty TenantIdentifier is the default-tenant unsuffixed naming path.
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		TenantIdentifier: "",
	}

	t.Run("absent configmap allows", func(t *testing.T) {
		t.Parallel()
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationAllow, got.Decision)
		assert.Equal(t, "absent", got.Baseline)
	})

	t.Run("default allows", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
			Data: map[string]string{
				ippPreProcessingConfigKey: defaultPreProcessingYAML,
				ippProcessingConfigKey:    defaultProcessingYAML,
			},
		}
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationAllow, got.Decision)
		assert.Equal(t, "default", got.Baseline)
	})

	t.Run("non-standard blocks without force", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
			Data: map[string]string{
				ippPreProcessingConfigKey: defaultPreProcessingYAML,
				ippProcessingConfigKey: `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: totally-custom
profiles:
- name: default
  plugins:
    request:
    - pluginRef: totally-custom
    response: []
`,
			},
		}
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationBlock, got.Decision)
		assert.Contains(t, got.Reason, AnnotationForcePayloadProcessingMigration)
	})

	t.Run("force annotation allows non-standard", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
			Data: map[string]string{
				ippPreProcessingConfigKey: defaultPreProcessingYAML,
				ippProcessingConfigKey: `apiVersion: llm-d.ai/v1alpha1
kind: PayloadProcessorConfig
plugins:
- type: totally-custom
profiles:
- name: default
  plugins:
    request:
    - pluginRef: totally-custom
`,
			},
		}
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace: "rhoai",
				Annotations: map[string]string{
					AnnotationForcePayloadProcessingMigration: "true",
				},
			},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationAllow, got.Decision)
		assert.Equal(t, "force", got.Baseline)
	})

	t.Run("praxis-shaped allows", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
			Data: map[string]string{
				praxisPreExtProcConfigKey: "server: {}",
				praxisExtProcConfigKey:    "server: {}",
			},
		}
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationAllow, got.Decision)
		assert.Equal(t, "praxis", got.Baseline)
	})

	t.Run("mixed ipp and praxis keys block", func(t *testing.T) {
		t.Parallel()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PayloadProcessingPluginsConfigMapName,
				Namespace: "openshift-ingress",
			},
			Data: map[string]string{
				praxisPreExtProcConfigKey: "server: {}",
				praxisExtProcConfigKey:    "server: {}",
				ippPreProcessingConfigKey: defaultPreProcessingYAML,
				ippProcessingConfigKey:    defaultProcessingYAML,
			},
		}
		tenant := &maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "rhoai"},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, tenant).Build()
		got, err := evaluatePluginsConfigMapMigration(context.Background(), cl, params, tenant)
		require.NoError(t, err)
		assert.Equal(t, pluginsMigrationBlock, got.Decision)
		assert.Contains(t, got.Reason, "non-standard")
	})
}
