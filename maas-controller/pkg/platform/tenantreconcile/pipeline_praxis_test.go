package tenantreconcile

import (
	"context"
	"encoding/json"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

type appliedResource struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

func praxisTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gwapiv1.Install(scheme))
	return scheme
}

func platformOverlayManifestPath(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := goruntime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(
		filepath.Dir(currentFile),
		"..", "..", "..", "..",
		"maas-api", "deploy", "overlays", "odh",
	))
}

func readyMaaSAPIDeployment(namespace, tenantID string) *appsv1.Deployment {
	replicas := int32(1)
	name := MaaSAPIDeploymentName(tenantID)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "maas-api", Image: "test"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
			ReadyReplicas:      1,
			Replicas:           1,
		},
	}
}

func praxisTenantConfig(namespace, tenantName string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedByAITenant: "true",
				LabelTenantName:        tenantName,
				LabelTenantNamespace:   namespace,
			},
			Annotations: map[string]string{
				AnnotationAITenantName:      tenantName,
				AnnotationAITenantNamespace: DefaultAITenantNamespace,
			},
		},
	}
}

func runPlatformTestClient(
	t *testing.T,
	scheme *runtime.Scheme,
	seed []client.Object,
	recordApplied *[]appliedResource,
) client.Client {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...)
	if recordApplied != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok {
					*recordApplied = append(*recordApplied, appliedResource{
						gvk:       u.GroupVersionKind(),
						namespace: u.GetNamespace(),
						name:      u.GetName(),
					})
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
	}
	return builder.Build()
}

func TestRunPlatform_PraxisSkipsIPPApply(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	var applied []appliedResource
	cl := runPlatformTestClient(t, scheme, []client.Object{
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName),
	}, &applied)

	result, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.DeploymentPending, "praxis tenant should not wait on IPP EnvoyFilter: %s", result.Detail)

	hasMaaSAPIDeployment := false
	for _, res := range applied {
		if res.gvk == GVKDeployment && strings.HasPrefix(res.name, baseMaaSAPIDeploymentName) {
			hasMaaSAPIDeployment = true
		}
		if isIPPResource(res.gvk, res.name) {
			t.Fatalf("unexpected IPP resource applied for praxis tenant: %s %s/%s", res.gvk.String(), res.namespace, res.name)
		}
	}
	assert.True(t, hasMaaSAPIDeployment, "expected maas-api Deployment to be applied")
}

func TestRunPlatform_PraxisCleansUpLegacyIPPResources(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	legacyDeployment := unstructuredIPPObject(GVKDeployment, gwNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(legacyDeployment, mcfg.UID)
	legacyEnvoyFilter := unstructuredIPPObject(GVKEnvoyFilter, gwNS, PayloadProcessingEnvoyFilterName(tenantName), nil)
	setConfigControllerOwnerRef(legacyEnvoyFilter, mcfg.UID)
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), legacyDeployment, legacyEnvoyFilter,
	).Build()

	result, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	depKey := types.NamespacedName{
		Namespace: gwNS,
		Name:      PayloadProcessingDeploymentName(tenantName),
	}
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(GVKDeployment)
	err = cl.Get(context.Background(), depKey, dep)
	require.True(t, apierrors.IsNotFound(err))

	efKey := types.NamespacedName{
		Namespace: gwNS,
		Name:      PayloadProcessingEnvoyFilterName(tenantName),
	}
	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(GVKEnvoyFilter)
	err = cl.Get(context.Background(), efKey, ef)
	require.True(t, apierrors.IsNotFound(err))

	gotTenant := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: appNs, Name: maasv1alpha1.MaasTenantConfigInstanceName}, gotTenant))
	assert.Equal(t, "true", gotTenant.Annotations[AnnotationIPPMigrationCleanupComplete])
}

func TestRunPlatform_PraxisMigrationCleanupSkipsPraxisOwnedResources(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	praxisDeployment := praxisOwnedDeployment(gwNS, PayloadProcessingDeploymentName(tenantName))
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), praxisDeployment,
	).Build()

	_, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)

	depKey := types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingDeploymentName(tenantName)}
	got := &appsv1.Deployment{}
	require.NoError(t, cl.Get(context.Background(), depKey, got), "praxis-owned deployment should survive migration cleanup")
}

func TestRunPlatform_PraxisSkipsCleanupAfterMigrationComplete(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	tenant.Annotations[AnnotationIPPMigrationCleanupComplete] = "true"
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	legacyDeployment := unstructuredIPPObject(GVKDeployment, gwNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(legacyDeployment, mcfg.UID)
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), legacyDeployment,
	).Build()

	_, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)

	depKey := types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingDeploymentName(tenantName)}
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(GVKDeployment)
	require.NoError(t, cl.Get(context.Background(), depKey, dep))
}

func TestRunPlatform_LegacyTenantAppliesIPPResources(t *testing.T) {
	const (
		tenantName = "legacy-team"
		appNs      = "ai-tenant-legacy-team"
		gwNS       = "openshift-ingress"
		gwName     = "legacy-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		Source:     "aitenant",
	}

	var applied []appliedResource
	cl := runPlatformTestClient(t, scheme, []client.Object{
		mcfg, gateway, readyMaaSAPIDeployment(appNs, tenantName),
	}, &applied)

	result, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.DeploymentPending, result.Detail)

	hasIPPDeployment := false
	hasIPPEnvoyFilter := false
	for _, res := range applied {
		if res.gvk == GVKDeployment && res.name == PayloadProcessingDeploymentName(tenantName) {
			hasIPPDeployment = true
		}
		if res.gvk == GVKEnvoyFilter && res.name == PayloadProcessingEnvoyFilterName(tenantName) {
			hasIPPEnvoyFilter = true
		}
	}
	assert.True(t, hasIPPDeployment, "expected payload-processing Deployment to be applied for legacy tenant")
	assert.True(t, hasIPPEnvoyFilter, "expected payload-processing EnvoyFilter to be applied for legacy tenant")
}

func TestRunPlatform_LegacyTenantReadyWithIPPEnvoyFilter(t *testing.T) {
	const (
		tenantName = "legacy-team"
		appNs      = "ai-tenant-legacy-team"
		gwNS       = "openshift-ingress"
		gwName     = "legacy-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	priority := PayloadProcessingEnvoyFilterPriority
	ef := payloadProcessingEnvoyFilter(gwNS, PayloadProcessingEnvoyFilterName(tenantName), gwName, &priority)
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		Source:     "aitenant",
	}

	cl := runPlatformTestClient(t, scheme, []client.Object{
		mcfg, gateway, readyMaaSAPIDeployment(appNs, tenantName), ef,
	}, nil)

	result, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.DeploymentPending, result.Detail)
}

func unstructuredIPPObject(gvk schema.GroupVersionKind, namespace, name string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	return obj
}

func praxisOwnedDeployment(namespace, name string) *appsv1.Deployment {
	fieldV1, err := json.Marshal(map[string]any{
		"f:metadata": map[string]any{
			"f:name": map[string]any{},
		},
	})
	if err != nil {
		panic(err)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager:    aiGatewayControllerFieldOwner,
				Operation:  metav1.ManagedFieldsOperationApply,
				APIVersion: "apps/v1",
				FieldsType: "FieldsV1",
				FieldsV1:   &metav1.FieldsV1{Raw: fieldV1},
			}},
		},
	}
}

func TestIPPResourcesForTenant_DefaultTenantUsesLegacyNames(t *testing.T) {
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		TenantIdentifier: "",
	}
	refs := ippResourcesForTenant(params)
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.name)
	}
	assert.Contains(t, names, PayloadProcessingName)
	assert.NotContains(t, names, PayloadProcessingDeploymentName("redteam"))
}

func TestIPPResourcesForTenant_PerTenantSuffix(t *testing.T) {
	const tenantID = "praxis-team"
	params := PlatformParams{
		GatewayNamespace: "openshift-ingress",
		TenantIdentifier: tenantID,
	}
	refs := ippResourcesForTenant(params)
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.name)
		if ref.gvk != GVKClusterRoleBinding {
			assert.Equal(t, "openshift-ingress", ref.namespace, "namespaced IPP ref %s", ref.name)
		}
	}
	assert.Contains(t, names, PayloadProcessingDeploymentName(tenantID))
	assert.Contains(t, names, PayloadProcessingEnvoyFilterName(tenantID))
	assert.Contains(t, names, PayloadProcessingReaderClusterRoleBindingNameForTenant(tenantID))
}

func TestCleanupIPPResources_DeletesManagedResources(t *testing.T) {
	const (
		tenantID = "praxis-team"
		gwNS     = "openshift-ingress"
	)
	params := PlatformParams{
		GatewayNamespace: gwNS,
		TenantIdentifier: tenantID,
	}
	scheme := praxisTestScheme(t)

	seed := []client.Object{
		unstructuredIPPObject(GVKDeployment, gwNS, PayloadProcessingDeploymentName(tenantID), nil),
		unstructuredIPPObject(GVKEnvoyFilter, gwNS, PayloadProcessingEnvoyFilterName(tenantID), nil),
		unstructuredIPPObject(GVKService, gwNS, PayloadProcessingServiceName(tenantID), nil),
	}
	for i := range seed {
		if obj, ok := seed[i].(*unstructured.Unstructured); ok {
			setConfigControllerOwnerRef(obj, types.UID("cfg-uid"))
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).Build()

	err := cleanupIPPResources(context.Background(), cl, params, types.UID("cfg-uid"), logr.Discard())
	require.NoError(t, err)

	for _, ref := range []ippResourceRef{
		{gvk: GVKDeployment, namespace: gwNS, name: PayloadProcessingDeploymentName(tenantID)},
		{gvk: GVKEnvoyFilter, namespace: gwNS, name: PayloadProcessingEnvoyFilterName(tenantID)},
		{gvk: GVKService, namespace: gwNS, name: PayloadProcessingServiceName(tenantID)},
	} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(ref.gvk)
		obj.SetName(ref.name)
		obj.SetNamespace(ref.namespace)
		err := cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		assert.True(t, apierrors.IsNotFound(err), "expected %s/%s to be deleted", ref.namespace, ref.name)
	}
}

func TestCleanupIPPResources_SkipsUnmanagedResources(t *testing.T) {
	const (
		tenantID = "praxis-team"
		gwNS     = "openshift-ingress"
	)
	params := PlatformParams{
		GatewayNamespace: gwNS,
		TenantIdentifier: tenantID,
	}
	scheme := praxisTestScheme(t)

	deployment := unstructuredIPPObject(
		GVKDeployment,
		gwNS,
		PayloadProcessingDeploymentName(tenantID),
		map[string]string{AnnotationManaged: "false"},
	)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()

	err := cleanupIPPResources(context.Background(), cl, params, types.UID("cfg-uid"), logr.Discard())
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKDeployment)
	key := types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingDeploymentName(tenantID)}
	require.NoError(t, cl.Get(context.Background(), key, got))
}

func TestCleanupIPPResources_SkipsPraxisOwnedResources(t *testing.T) {
	const (
		tenantID = "praxis-team"
		gwNS     = "openshift-ingress"
	)
	params := PlatformParams{
		GatewayNamespace: gwNS,
		TenantIdentifier: tenantID,
	}
	scheme := praxisTestScheme(t)

	deployment := praxisOwnedDeployment(gwNS, PayloadProcessingDeploymentName(tenantID))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()

	err := cleanupIPPResources(context.Background(), cl, params, types.UID("cfg-uid"), logr.Discard())
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKDeployment)
	key := types.NamespacedName{Namespace: gwNS, Name: PayloadProcessingDeploymentName(tenantID)}
	require.NoError(t, cl.Get(context.Background(), key, got))
}
