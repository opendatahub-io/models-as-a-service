package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metamapping "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

type failingExternalModelReader struct {
	client.Reader
	err error
}

func (r failingExternalModelReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if models, ok := list.(*unstructured.UnstructuredList); ok && models.GetKind() == "ExternalModelList" {
		return r.err
	}
	return r.Reader.List(ctx, list, opts...)
}

type failingMigrationAnnotationClient struct{ client.Client }

func (c failingMigrationAnnotationClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*maasv1alpha1.MaasTenantConfig); ok {
		return errors.New("migration annotation write denied")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestRunPlatformBlocksMigrationWhenExternalModelAssessmentFails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "list failure", err: errors.New("ExternalModel list unavailable")},
		{name: "unknown discovery", err: &metamapping.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: "inference.opendatahub.io", Kind: "ExternalModel"},
			SearchedVersions: []string{"v1alpha1"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const (
				tenantName = "translation-team"
				appNS      = "ai-tenant-translation-team"
				gatewayNS  = "openshift-ingress"
				gateway    = "translation-gateway"
			)
			scheme := praxisTestScheme(t)
			tenant := praxisTenantConfig(appNS, tenantName)
			config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")}}
			plugins := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: PayloadProcessingPluginsConfigMapForTenant(tenantName), Namespace: gatewayNS,
			}, Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: defaultProcessingYAML}}
			deployment := unstructuredIPPObject(GVKDeployment, gatewayNS, PayloadProcessingDeploymentName(tenantName), nil)
			setConfigControllerOwnerRef(deployment, config.UID)
			base := newSSAFakeClientBuilder(scheme).WithObjects(
				config, &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gateway, Namespace: gatewayNS}},
				tenant, readyMaaSAPIDeployment(appNS, tenantName), plugins, deployment,
			).Build()
			reader := failingExternalModelReader{Reader: base, err: test.err}
			platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gatewayNS, Name: gateway}, SkipIPP: true, Source: "aitenant"}
			result, err := RunPlatformWithReader(context.Background(), logr.Discard(), base, reader, scheme, tenant,
				platformContext, platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.DeploymentPending, "uncertain provider state must block migration")
			require.Contains(t, result.Detail, "cannot assess legacy IPP translation compatibility")
			require.Contains(t, result.Detail, test.err.Error())
			require.Contains(t, result.Detail, "keeping the IPP ConfigMap and resources")
			require.NotEmpty(t, result.Warnings)
			// Detail may prepend EnvoyFilter readiness; Warnings retain the migration block reason.
			require.Contains(t, result.Detail, result.Warnings[0])

			stored := &maasv1alpha1.MaasTenantConfig{}
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
			require.Equal(t, PayloadProcessingMigrationBlocked, stored.Annotations[AnnotationPayloadProcessingMigration])
			require.Empty(t, stored.Annotations[AnnotationPayloadProcessingStatus])
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(plugins), &corev1.ConfigMap{}))
			remaining := unstructuredIPPObject(GVKDeployment, gatewayNS, deployment.GetName(), nil)
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(deployment), remaining))
			require.NoError(t, base.Get(context.Background(), client.ObjectKey{Namespace: appNS, Name: MaaSAPIServiceName(tenantName)}, &corev1.Service{}),
				"non-IPP resources must still be applied")
		})
	}
}

func TestMigrationAssessmentBlockReportsAnnotationWriteFailure(t *testing.T) {
	const (
		tenantName = "translation-team"
		appNS      = "ai-tenant-translation-team"
		gatewayNS  = "openshift-ingress"
	)
	tenant := praxisTenantConfig(appNS, tenantName)
	plugins := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: PayloadProcessingPluginsConfigMapForTenant(tenantName), Namespace: gatewayNS,
	}, Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: defaultProcessingYAML}}
	base := newSSAFakeClientBuilder(praxisTestScheme(t)).WithObjects(tenant, plugins).Build()
	reader := failingExternalModelReader{Reader: base, err: errors.New("ExternalModel list unavailable")}
	params := PlatformParams{SkipIPP: true, TenantIdentifier: tenantName, GatewayNamespace: gatewayNS, ModelNamespace: appNS}
	_, err := reconcilePayloadProcessingSwitch(context.Background(), failingMigrationAnnotationClient{Client: base}, reader,
		tenant, &params, nil, logr.Discard())
	require.ErrorContains(t, err, "mark API translation compatibility migration blocked")
	require.ErrorContains(t, err, "migration annotation write denied")
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(plugins), &corev1.ConfigMap{}),
		"annotation failure must not remove the IPP ConfigMap")
}

func TestRunPlatformPersistsResponseTranslationBeforeCleanupAndRetriesAfterFailure(t *testing.T) {
	const (
		tenantName = "translation-team"
		appNS      = "ai-tenant-translation-team"
		gatewayNS  = "openshift-ingress"
		gateway    = "translation-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNS, tenantName)
	config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")}}
	owner := true
	plugins := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PayloadProcessingPluginsConfigMapForTenant(tenantName),
			Namespace:       gatewayNS,
			UID:             types.UID("plugins-uid"),
			ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: config.Name, UID: config.UID, Controller: &owner}},
		},
		Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: responseAPITranslationProcessingYAML},
	}
	deployment := unstructuredIPPObject(GVKDeployment, gatewayNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(deployment, config.UID)
	base := newSSAFakeClientBuilder(scheme).WithObjects(
		config, &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gateway, Namespace: gatewayNS}}, tenant,
		readyMaaSAPIDeployment(appNS, tenantName), plugins, deployment,
	).Build()
	cl := &translationCleanupClient{Client: base, configMapName: plugins.Name, failOnce: true}
	platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gatewayNS, Name: gateway}, SkipIPP: true, Source: "aitenant"}

	run := func(current *maasv1alpha1.MaasTenantConfig) (*RunResult, error) {
		return RunPlatform(context.Background(), logr.Discard(), cl, scheme, current, platformContext,
			platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
	}
	_, err := run(tenant)
	require.ErrorContains(t, err, "injected cleanup failure")
	require.True(t, cl.translationWasPersistedAtDelete, "cleanup was attempted before durable translation intent")
	require.True(t, cl.deletedConfigMapAttempted)

	stored := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	require.NotNil(t, stored.Spec.APITranslation)
	require.NotNil(t, stored.Spec.APITranslation.Enabled)
	require.True(t, *stored.Spec.APITranslation.Enabled)
	remaining := &corev1.ConfigMap{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining), "failed deletion must leave the recognized IPP ConfigMap")

	// A controller restart/reconcile uses the persisted value as an existing
	// choice, finishes cleanup, and does not need the deleted ConfigMap to infer it.
	result, err := run(stored)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, cl.translationWasPersistedAtDelete)
	err = base.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining)
	require.True(t, apierrors.IsNotFound(err), "retry should finish IPP cleanup")
	stored = &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	require.Equal(t, PayloadProcessingStatusCleanupComplete, stored.Annotations[AnnotationPayloadProcessingStatus])
}

func TestRunPlatformGatesOnlyUnsupportedExternalModelHandoffs(t *testing.T) {
	for _, test := range []struct {
		name      string
		apiFormat string
		provider  string
		setting   *bool
		wantBlock bool
	}{
		{name: "Anthropic provider blocks pure-default cleanup", apiFormat: "messages", provider: "anthropic", wantBlock: true},
		{name: "explicit false accepts compatibility risk", apiFormat: "messages", provider: "anthropic", setting: migrationBoolPtr(false)},
		{name: "OpenAI provider Chat Completions passthrough remains migratable", apiFormat: "openai-chat", provider: "openai"},
		{name: "Azure OpenAI wire format still requires compatibility review", apiFormat: "openai-chat", provider: "azure", wantBlock: true},
		{name: "Vertex OpenAI wire format still requires compatibility review", apiFormat: "openai-chat", provider: "vertex", wantBlock: true},
		{name: "Bedrock OpenAI wire format still requires compatibility review", apiFormat: "openai-chat", provider: "aws-bedrock", wantBlock: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const (
				tenantName = "translation-team"
				appNS      = "ai-tenant-translation-team"
				gatewayNS  = "openshift-ingress"
				gateway    = "translation-gateway"
			)
			scheme := praxisTestScheme(t)
			tenant := praxisTenantConfig(appNS, tenantName)
			if test.setting != nil {
				tenant.Spec.APITranslation = &maasv1alpha1.TenantAPITranslationConfig{Enabled: test.setting}
			}
			config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")}}
			owner := true
			plugins := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: PayloadProcessingPluginsConfigMapForTenant(tenantName), Namespace: gatewayNS,
				UID: types.UID("plugins-uid"), ResourceVersion: "1",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: config.Name, UID: config.UID, Controller: &owner}},
			}, Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: defaultProcessingYAML}}
			deployment := unstructuredIPPObject(GVKDeployment, gatewayNS, PayloadProcessingDeploymentName(tenantName), nil)
			setConfigControllerOwnerRef(deployment, config.UID)
			model := legacyTranslationModel(appNS, "external-model", test.apiFormat, gateway, gatewayNS, nil)
			provider := legacyTranslationProvider(appNS, "external-model-provider", test.provider)
			base := newSSAFakeClientBuilder(scheme).WithObjects(
				config, &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gateway, Namespace: gatewayNS}},
				tenant, readyMaaSAPIDeployment(appNS, tenantName), plugins, deployment, model, provider,
			).Build()
			platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gatewayNS, Name: gateway}, SkipIPP: true, Source: "aitenant"}
			result, err := RunPlatform(context.Background(), logr.Discard(), base, scheme, tenant, platformContext,
				platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
			require.NoError(t, err)
			require.NotNil(t, result)
			remaining := &corev1.ConfigMap{}
			err = base.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining)
			if test.wantBlock {
				require.True(t, result.DeploymentPending)
				require.Contains(t, result.Detail, "cannot preserve OpenAI Chat Completions")
				require.NoError(t, err, "blocked migration must keep the IPP ConfigMap")
				stored := &maasv1alpha1.MaasTenantConfig{}
				require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
				require.Equal(t, PayloadProcessingMigrationBlocked, stored.Annotations[AnnotationPayloadProcessingMigration])
				require.Nil(t, stored.Spec.APITranslation, "pure-default migration must not invent response translation intent")
				return
			}
			require.True(t, apierrors.IsNotFound(err), "safe or explicitly accepted handoff should finish cleanup")
			if test.setting != nil {
				require.NotEmpty(t, result.Warnings)
				require.Contains(t, result.Warnings[0], "accepts this compatibility loss")
				stored := &maasv1alpha1.MaasTenantConfig{}
				require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
				require.Equal(t, APITranslationMigrationRiskExplicitFalse, stored.Annotations[AnnotationAPITranslationMigrationRisk])

				// A later steady-state reconcile must keep the accepted risk visible
				// after Praxis claims the handoff and the one-shot IPP cleanup is done.
				stored.Annotations[AnnotationPayloadProcessingStatus] = PayloadProcessingStatusSteady
				require.NoError(t, base.Update(context.Background(), stored))
				stored = &maasv1alpha1.MaasTenantConfig{}
				require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
				steadyResult, err := RunPlatform(context.Background(), logr.Discard(), base, scheme, stored, platformContext,
					platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
				require.NoError(t, err)
				require.Contains(t, steadyResult.Warnings[0], "accepts this compatibility loss")
			}
		})
	}
}

func TestRunPlatformPersistsAcceptedRiskBeforeCleanupFailureAndKeepsWarningAfterRetry(t *testing.T) {
	const (
		tenantName = "translation-team"
		appNS      = "ai-tenant-translation-team"
		gatewayNS  = "openshift-ingress"
		gateway    = "translation-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNS, tenantName)
	tenant.Spec.APITranslation = &maasv1alpha1.TenantAPITranslationConfig{Enabled: migrationBoolPtr(false)}
	config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")}}
	owner := true
	plugins := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: PayloadProcessingPluginsConfigMapForTenant(tenantName), Namespace: gatewayNS,
		UID: types.UID("plugins-uid"), ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: config.Name, UID: config.UID, Controller: &owner}},
	}, Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: defaultProcessingYAML}}
	deployment := unstructuredIPPObject(GVKDeployment, gatewayNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(deployment, config.UID)
	model := legacyTranslationModel(appNS, "external-model", "messages", gateway, gatewayNS, nil)
	provider := legacyTranslationProvider(appNS, "external-model-provider", "anthropic")
	base := newSSAFakeClientBuilder(scheme).WithObjects(
		config, &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gateway, Namespace: gatewayNS}}, tenant,
		readyMaaSAPIDeployment(appNS, tenantName), plugins, deployment, model, provider,
	).Build()
	cl := &translationCleanupClient{Client: base, configMapName: plugins.Name, failOnce: true}
	platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gatewayNS, Name: gateway}, SkipIPP: true, Source: "aitenant"}
	run := func(current *maasv1alpha1.MaasTenantConfig) (*RunResult, error) {
		return RunPlatform(context.Background(), logr.Discard(), cl, scheme, current, platformContext,
			platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
	}

	_, err := run(tenant)
	require.ErrorContains(t, err, "injected cleanup failure")
	require.True(t, cl.riskWasPersistedAtDelete, "the accepted risk must be durable before destructive cleanup starts")
	remaining := &corev1.ConfigMap{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining), "failed deletion must leave the IPP ConfigMap")

	stored := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	require.Equal(t, APITranslationMigrationRiskExplicitFalse, stored.Annotations[AnnotationAPITranslationMigrationRisk])
	result, err := run(stored)
	require.NoError(t, err)
	require.Contains(t, result.Warnings[0], "explicit spec.apiTranslation.enabled=false accepts this compatibility loss")
	require.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining)))

	stored = &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	stored.Annotations[AnnotationPayloadProcessingStatus] = PayloadProcessingStatusSteady
	require.NoError(t, base.Update(context.Background(), stored))
	stored = &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(tenant), stored))
	steadyResult, err := run(stored)
	require.NoError(t, err)
	require.Contains(t, steadyResult.Warnings[0], "explicit spec.apiTranslation.enabled=false accepts this compatibility loss")
}

func TestRunPlatformUsesLiveReaderBeforeIPPTranslationCleanup(t *testing.T) {
	const (
		tenantName = "translation-team"
		appNS      = "ai-tenant-translation-team"
		gatewayNS  = "openshift-ingress"
		gateway    = "translation-gateway"
	)
	scheme := praxisTestScheme(t)
	config := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")}}
	tenant := praxisTenantConfig(appNS, tenantName)
	owner := true
	plugins := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            PayloadProcessingPluginsConfigMapForTenant(tenantName),
			Namespace:       gatewayNS,
			UID:             types.UID("plugins-uid"),
			ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: config.Name, UID: config.UID, Controller: &owner}},
		},
		Data: map[string]string{ippPreProcessingConfigKey: defaultPreProcessingYAML, ippProcessingConfigKey: defaultProcessingYAML},
	}
	deployment := unstructuredIPPObject(GVKDeployment, gatewayNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(deployment, config.UID)
	gatewayObj := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gateway, Namespace: gatewayNS}}
	readyAPI := readyMaaSAPIDeployment(appNS, tenantName)
	staleModel := legacyTranslationModel(appNS, "external-model", "openai-chat", gateway, gatewayNS, nil)
	liveModel := legacyTranslationModel(appNS, "external-model", "messages", gateway, gatewayNS, nil)
	staleProvider := legacyTranslationProvider(appNS, "external-model-provider", "openai")
	liveProvider := legacyTranslationProvider(appNS, "external-model-provider", "anthropic")

	cachedClient := newSSAFakeClientBuilder(scheme).WithObjects(
		config, gatewayObj, tenant, readyAPI, plugins, deployment, staleModel, staleProvider,
	).Build()
	apiReader := newSSAFakeClientBuilder(scheme).WithObjects(
		config, gatewayObj, tenant.DeepCopy(), readyAPI.DeepCopy(), plugins.DeepCopy(), deployment.DeepCopy(), liveModel, liveProvider,
	).Build()
	platformContext := PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gatewayNS, Name: gateway}, SkipIPP: true, Source: "aitenant"}

	result, err := RunPlatformWithReader(context.Background(), logr.Discard(), cachedClient, apiReader, scheme, tenant,
		platformContext, platformOverlayManifestPath(t), appNS, "controller-ns", "https://kubernetes.default.svc", "opendatahub", config)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.DeploymentPending)
	require.Contains(t, result.Detail, "cannot preserve OpenAI Chat Completions")
	remaining := &corev1.ConfigMap{}
	require.NoError(t, cachedClient.Get(context.Background(), client.ObjectKeyFromObject(plugins), remaining),
		"the live messages binding must keep the IPP ConfigMap even when the cached ExternalModel is stale")
}

type translationCleanupClient struct {
	client.Client
	configMapName                   string
	failOnce                        bool
	deletedConfigMapAttempted       bool
	translationWasPersistedAtDelete bool
	riskWasPersistedAtDelete        bool
}

func (c *translationCleanupClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" && obj.GetName() == c.configMapName {
		c.deletedConfigMapAttempted = true
		stored := &maasv1alpha1.MaasTenantConfig{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ai-tenant-translation-team", Name: maasv1alpha1.MaasTenantConfigInstanceName}, stored); err == nil &&
			stored.Spec.APITranslation != nil && stored.Spec.APITranslation.Enabled != nil {
			c.translationWasPersistedAtDelete = *stored.Spec.APITranslation.Enabled
		}
		stored = &maasv1alpha1.MaasTenantConfig{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ai-tenant-translation-team", Name: maasv1alpha1.MaasTenantConfigInstanceName}, stored); err == nil {
			c.riskWasPersistedAtDelete = stored.Annotations[AnnotationAPITranslationMigrationRisk] != ""
		}
		if c.failOnce {
			c.failOnce = false
			return errors.New("injected cleanup failure")
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}
