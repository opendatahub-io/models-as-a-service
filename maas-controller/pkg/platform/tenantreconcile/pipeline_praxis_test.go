package tenantreconcile

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRunPlatform_PraxisCleansUpExistingIPPResources(t *testing.T) {
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
	ippDeployment := unstructuredIPPObject(GVKDeployment, gwNS, PayloadProcessingDeploymentName(tenantName), nil)
	setConfigControllerOwnerRef(ippDeployment, mcfg.UID)
	ippEnvoyFilter := unstructuredIPPObject(GVKEnvoyFilter, gwNS, PayloadProcessingEnvoyFilterName(tenantName), nil)
	setConfigControllerOwnerRef(ippEnvoyFilter, mcfg.UID)
	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), ippDeployment, ippEnvoyFilter,
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

func TestRunPlatform_DefaultTenantAppliesIPPResources(t *testing.T) {
	const (
		tenantName = "existing-team"
		appNs      = "ai-tenant-existing-team"
		gwNS       = "openshift-ingress"
		gwName     = "existing-gateway"
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
	assert.True(t, hasIPPDeployment, "expected payload-processing Deployment to be applied for the existing IPP path")
	assert.True(t, hasIPPEnvoyFilter, "expected payload-processing EnvoyFilter to be applied for the existing IPP path")
}

func TestRunPlatform_DefaultTenantReadyWithIPPEnvoyFilter(t *testing.T) {
	const (
		tenantName = "existing-team"
		appNs      = "ai-tenant-existing-team"
		gwNS       = "openshift-ingress"
		gwName     = "existing-gateway"
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
	obj.SetUID(types.UID("uid-" + name))
	obj.SetResourceVersion("1")
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

func TestIPPResourcesForTenant_DefaultTenantUsesExistingNames(t *testing.T) {
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
		ModelNamespace:   "tenant-ns",
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
		ModelNamespace:   "tenant-ns",
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
		ModelNamespace:   "tenant-ns",
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

func TestCleanupIPPResources_DeletesOnlyOwnedInferenceRouteAfterWritersStop(t *testing.T) {
	const ns = "llm"
	params := PlatformParams{AppNamespace: "maas-system", ModelNamespace: ns, GatewayNamespace: "openshift-ingress", TenantIdentifier: "team-a"}
	scheme := praxisTestScheme(t)
	uid := types.UID("inference-uid")
	model := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalModel",
		"metadata": map[string]any{"name": "demo", "namespace": ns, "uid": string(uid)},
	}}
	model.SetGroupVersionKind(inferenceExternalModelRouteOwnerGVK)
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{
			"name": "demo", "namespace": ns,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": ippExternalModelManagedBy,
				ippExternalModelLabel:          "demo",
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalModel", "name": "demo", "uid": string(uid), "controller": true,
			}},
		},
	}}
	route.SetGroupVersionKind(GVKHTTPRoute)
	route.SetUID(types.UID("route-uid"))
	route.SetResourceVersion("1")
	userRoute := route.DeepCopy()
	userRoute.SetName("user-route")
	userRoute.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "user"})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model, route, userRoute).Build()
	require.NoError(t, cleanupIPPResources(context.Background(), cl, params, types.UID("cfg"), logr.Discard()))
	deleted := &unstructured.Unstructured{}
	deleted.SetGroupVersionKind(GVKHTTPRoute)
	assert.True(t, apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "demo"}, deleted)))
	assert.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "user-route"}, deleted))
}

func TestCleanupIPPResources_DefersRouteWhileIPPWriterExists(t *testing.T) {
	const (
		ns        = "llm"
		gatewayNS = "openshift-ingress"
		tenantID  = "team-a"
	)
	params := PlatformParams{AppNamespace: "maas-system", ModelNamespace: ns, GatewayNamespace: gatewayNS, TenantIdentifier: tenantID}
	scheme := praxisTestScheme(t)
	model := ippExternalModel(ns, "demo", types.UID("model-uid"))
	route := ippOwnedExternalModelRoute(ns, "demo", types.UID("model-uid"))
	writer := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "writer-pod",
		Namespace: gatewayNS,
		Labels: map[string]string{
			LabelTenantInstance: PayloadProcessingDeploymentName(tenantID),
			"app":               PayloadProcessingName,
		},
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model, route, writer).Build()
	err := cleanupIPPResources(context.Background(), cl, params, types.UID("cfg"), logr.Discard())
	assert.ErrorContains(t, err, "writer pod")
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKHTTPRoute)
	assert.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "demo"}, got))
}

func ippOwnedExternalModelRoute(namespace, modelName string, uid types.UID) *unstructured.Unstructured {
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{
			"name": modelName, "namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": ippExternalModelManagedBy,
				ippExternalModelLabel:          modelName,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalModel", "name": modelName, "uid": string(uid), "controller": true,
			}},
		},
	}}
	route.SetGroupVersionKind(GVKHTTPRoute)
	route.SetUID(types.UID("route-uid-" + modelName))
	route.SetResourceVersion("1")
	return route
}

func ippExternalModel(namespace, name string, uid types.UID) *unstructured.Unstructured {
	model := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inference.opendatahub.io/v1alpha1", "kind": "ExternalModel",
		"metadata": map[string]any{"name": name, "namespace": namespace, "uid": string(uid)},
	}}
	model.SetGroupVersionKind(inferenceExternalModelRouteOwnerGVK)
	return model
}

func TestCleanupIPPExternalModelRoutes_PreservesUnsafeOwnership(t *testing.T) {
	const ns = "llm"
	params := PlatformParams{AppNamespace: "maas-system", ModelNamespace: ns}
	cases := map[string]struct {
		model  bool
		mutate func(*unstructured.Unstructured)
	}{
		"recreated model UID": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			refs[0].UID = types.UID("new-uid")
			route.SetOwnerReferences(refs)
		}},
		"missing model": {model: false},
		"missing controller owner": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			falseValue := false
			refs[0].Controller = &falseValue
			route.SetOwnerReferences(refs)
		}},
		"empty owner UID": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			refs[0].UID = ""
			route.SetOwnerReferences(refs)
		}},
		"wrong owner API version": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			refs[0].APIVersion = "inference.opendatahub.io/v1beta1"
			route.SetOwnerReferences(refs)
		}},
		"wrong owner kind": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			refs[0].Kind = "ExternalProvider"
			route.SetOwnerReferences(refs)
		}},
		"wrong owner name": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			refs[0].Name = "other"
			route.SetOwnerReferences(refs)
		}},
		"duplicate matching owners": {model: true, mutate: func(route *unstructured.Unstructured) {
			refs := route.GetOwnerReferences()
			route.SetOwnerReferences(append(refs, refs[0]))
		}},
		"SSA managed": {model: true, mutate: func(route *unstructured.Unstructured) {
			route.SetManagedFields([]metav1.ManagedFieldsEntry{{
				Manager:    aiGatewayControllerFieldOwner,
				Operation:  metav1.ManagedFieldsOperationApply,
				APIVersion: "gateway.networking.k8s.io/v1",
				FieldsType: "FieldsV1",
				FieldsV1:   &metav1.FieldsV1{Raw: []byte("{}")},
			}})
		}},
		"foreign route": {model: true, mutate: func(route *unstructured.Unstructured) {
			route.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "user"})
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			route := ippOwnedExternalModelRoute(ns, "demo", types.UID("old-uid"))
			if tc.mutate != nil {
				tc.mutate(route)
			}
			if name == "SSA managed" {
				require.True(t, hasSSAFieldManager(route, aiGatewayControllerFieldOwner))
			}
			objects := []client.Object{route}
			if tc.model {
				objects = append(objects, ippExternalModel(ns, "demo", types.UID("old-uid")))
			}
			builder := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(objects...)
			if name == "SSA managed" {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if err := c.List(ctx, list, opts...); err != nil {
							return err
						}
						items, ok := list.(*unstructured.UnstructuredList)
						if ok && len(items.Items) == 1 {
							items.Items[0].SetManagedFields(route.GetManagedFields())
						}
						return nil
					},
				})
			}
			cl := builder.Build()
			require.NoError(t, cleanupIPPExternalModelRoutes(context.Background(), cl, params, logr.Discard()))
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(GVKHTTPRoute)
			assert.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "demo"}, got))
		})
	}
}

func TestEnsureIPPWritersStopped_DefersForTerminatingDeploymentAndRemainingPod(t *testing.T) {
	const (
		ns       = "openshift-ingress"
		tenantID = "team-a"
	)
	params := PlatformParams{GatewayNamespace: ns, TenantIdentifier: tenantID}
	deletion := metav1.Now()
	deployment := unstructuredIPPObject(GVKDeployment, ns, PayloadProcessingDeploymentName(tenantID), nil)
	setConfigControllerOwnerRef(deployment, types.UID("cfg"))
	deployment.SetFinalizers([]string{"test/finalizer"})
	deployment.SetDeletionTimestamp(&deletion)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "writer-pod", Namespace: ns,
		Labels: map[string]string{LabelTenantInstance: PayloadProcessingDeploymentName(tenantID)},
	}}
	cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(deployment, pod).Build()
	err := ensureIPPWritersStopped(context.Background(), cl, params, types.UID("cfg"))
	assert.ErrorContains(t, err, "terminating")

	cl = fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(pod).Build()
	err = ensureIPPWritersStopped(context.Background(), cl, params, types.UID("cfg"))
	assert.ErrorContains(t, err, "writer pod")
}

func TestEnsureIPPWritersStopped_SkipsPodsWhenPraxisOwnsDeployment(t *testing.T) {
	const (
		ns       = "openshift-ingress"
		tenantID = "team-a"
	)
	params := PlatformParams{GatewayNamespace: ns, TenantIdentifier: tenantID}
	deployment := praxisOwnedDeployment(ns, PayloadProcessingDeploymentName(tenantID))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "praxis-writer",
		Namespace: ns,
		Labels: map[string]string{
			LabelTenantInstance: PayloadProcessingDeploymentName(tenantID),
			"app":               PayloadProcessingName,
		},
	}}
	cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(deployment, pod).Build()
	err := ensureIPPWritersStopped(context.Background(), cl, params, types.UID("cfg"))
	assert.NoError(t, err)
}

func TestEnsureIPPWritersStoppedChecksPodsIndependentOfDeploymentOwnership(t *testing.T) {
	const (
		ns       = "openshift-ingress"
		tenantID = "team-a"
	)
	params := PlatformParams{GatewayNamespace: ns, TenantIdentifier: tenantID}
	pod := func(name, app string, extra map[string]string) *corev1.Pod {
		labels := map[string]string{
			LabelTenantInstance: PayloadProcessingDeploymentName(tenantID),
			"app":               app,
		}
		for key, value := range extra {
			labels[key] = value
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	}

	tests := map[string]struct {
		deployment client.Object
		pod        *corev1.Pod
		wantError  string
	}{
		"absent deployment with writer pod": {
			pod:       pod("writer", PayloadProcessingName, nil),
			wantError: "writer pod",
		},
		"foreign deployment with writer pod": {
			deployment: unstructuredIPPObject(GVKDeployment, ns, PayloadProcessingDeploymentName(tenantID), map[string]string{AnnotationManaged: "false"}),
			pod:        pod("writer", PayloadProcessingName, nil),
		},
		"Praxis deployment with writer pod": {
			deployment: func() client.Object {
				return praxisOwnedDeployment(ns, PayloadProcessingDeploymentName(tenantID))
			}(),
			pod: pod("writer", PayloadProcessingName, nil),
		},
		"foreign deployment without writer pod": {
			deployment: unstructuredIPPObject(GVKDeployment, ns, PayloadProcessingDeploymentName(tenantID), map[string]string{AnnotationManaged: "false"}),
		},
		"Praxis pod is excluded": {
			pod: pod("praxis", PayloadProcessingName, map[string]string{"app.kubernetes.io/managed-by": aiGatewayControllerFieldOwner}),
		},
		"unrelated tenant pod is excluded": {
			pod: pod("unrelated", "other-workload", nil),
		},
		"ambiguous tenant pod fails closed": {
			pod: func() *corev1.Pod {
				result := pod("ambiguous", "", nil)
				delete(result.Labels, "app")
				return result
			}(),
			wantError: "ambiguous identity",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.deployment != nil {
				objects = append(objects, tt.deployment)
			}
			if tt.pod != nil {
				objects = append(objects, tt.pod)
			}
			cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(objects...).Build()
			err := ensureIPPWritersStopped(context.Background(), cl, params, types.UID("cfg"))
			if tt.wantError == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantError)
			}
		})
	}
}

func TestRunPlatformDoesNotMarkCleanupCompleteWhileWriterPodRemains(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")}}
	gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "writer-pod",
		Namespace: gwNS,
		Labels: map[string]string{
			LabelTenantInstance: PayloadProcessingDeploymentName(tenantName),
			"app":               PayloadProcessingName,
		},
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), pod,
	).Build()

	_, err := RunPlatform(
		context.Background(), logr.Discard(), cl, scheme, tenant,
		PlatformContext{GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName}, SkipIPP: true, Source: "aitenant"},
		platformOverlayManifestPath(t), appNs, "controller-ns", "https://kubernetes.default.svc", "opendatahub", mcfg,
	)
	require.ErrorContains(t, err, "writer pod")
	persistedTenant := &maasv1alpha1.MaasTenantConfig{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(tenant), persistedTenant))
	assert.False(t, isIPPMigrationCleanupComplete(persistedTenant), "cleanup must not be marked complete while a writer remains")
}

func TestCleanupIPPExternalModelRoutes_IsNamespaceScoped(t *testing.T) {
	const (
		ns      = "llm"
		infraNS = "maas-system"
	)
	route := ippOwnedExternalModelRoute(ns, "demo", types.UID("uid"))
	sharedRoute := ippOwnedExternalModelRoute(infraNS, "shared", types.UID("shared-uid"))
	cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(
		route,
		sharedRoute,
		ippExternalModel(ns, "demo", types.UID("uid")),
		ippExternalModel(infraNS, "shared", types.UID("shared-uid")),
	).Build()
	require.NoError(t, cleanupIPPExternalModelRoutes(context.Background(), cl, PlatformParams{AppNamespace: infraNS, ModelNamespace: ns}, logr.Discard()))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKHTTPRoute)
	assert.True(t, apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "demo"}, got)))
	assert.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: infraNS, Name: "shared"}, got), "a model in shared infrastructure is not proven to belong exclusively to this tenant")
}

func TestCleanupIPPExternalModelRoutesRequiresModelNamespace(t *testing.T) {
	err := cleanupIPPExternalModelRoutes(context.Background(), fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).Build(), PlatformParams{AppNamespace: "maas-system"}, logr.Discard())
	assert.ErrorContains(t, err, "model namespace is required")
}

func TestDeleteIPPResourceIfManaged_UsesForegroundPropagationForWriters(t *testing.T) {
	const ns = "openshift-ingress"
	deployment := unstructuredIPPObject(GVKDeployment, ns, PayloadProcessingDeploymentName("team-a"), nil)
	setConfigControllerOwnerRef(deployment, types.UID("cfg"))
	var propagation *metav1.DeletionPropagation
	var preconditions *metav1.Preconditions
	cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(deployment).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
			propagation = deleteOptions.PropagationPolicy
			preconditions = deleteOptions.Preconditions
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	require.NoError(t, deleteIPPResourceIfManaged(context.Background(), cl, ippResourceRef{
		gvk: GVKDeployment, namespace: ns, name: deployment.GetName(),
	}, types.UID("cfg"), logr.Discard()))
	require.NotNil(t, propagation)
	assert.Equal(t, metav1.DeletePropagationForeground, *propagation)
	require.NotNil(t, preconditions)
	assert.Equal(t, deployment.GetUID(), *preconditions.UID)
	assert.Equal(t, deployment.GetResourceVersion(), *preconditions.ResourceVersion)
}

func TestCleanupIPPExternalModelRoutesPropagatesDeletePreconditionFailure(t *testing.T) {
	const ns = "llm"
	route := ippOwnedExternalModelRoute(ns, "demo", types.UID("uid"))
	model := ippExternalModel(ns, "demo", types.UID("uid"))
	conflict := apierrors.NewConflict(schema.GroupResource{Group: GVKHTTPRoute.Group, Resource: "httproutes"}, route.GetName(), errors.New("resource changed"))
	cl := fake.NewClientBuilder().WithScheme(praxisTestScheme(t)).WithObjects(route, model).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
			require.NotNil(t, deleteOptions.Preconditions)
			require.Equal(t, route.GetUID(), *deleteOptions.Preconditions.UID)
			require.Equal(t, route.GetResourceVersion(), *deleteOptions.Preconditions.ResourceVersion)
			return conflict
		},
	}).Build()

	err := cleanupIPPExternalModelRoutes(context.Background(), cl, PlatformParams{AppNamespace: "maas-system", ModelNamespace: ns}, logr.Discard())
	assert.ErrorIs(t, err, conflict)
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKHTTPRoute)
	assert.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: route.GetName()}, got))
}
