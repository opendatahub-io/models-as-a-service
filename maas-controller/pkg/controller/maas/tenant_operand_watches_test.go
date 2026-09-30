package maas

import (
	"context"
	"errors"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

const (
	operandInfraNS   = "odh-ai-gateway-infra"
	operandGatewayNS = "openshift-ingress"
	operandDefaultNS = "models-as-a-service"
	operandRedNS     = "ai-tenant-red"
	operandGateway   = "maas-default-gateway"
)

func operandTestReconciler(c client.Client, discovery bool) *TenantReconciler {
	return &TenantReconciler{
		Client:                          c,
		Scheme:                          scheme,
		AppNamespace:                    operandInfraNS,
		TenantNamespace:                 operandDefaultNS,
		GatewayName:                     operandGateway,
		GatewayNamespace:                operandGatewayNS,
		OperatorNamespace:               "opendatahub",
		TenantNamespaceDiscoveryEnabled: discovery,
	}
}

func trackedBy(tenantName, tenantNamespace string) map[string]string {
	return map[string]string{
		tenantreconcile.LabelTenantName:      tenantName,
		tenantreconcile.LabelTenantNamespace: tenantNamespace,
	}
}

func operandMeta(gvk schema.GroupVersionKind, namespace, name string, l map[string]string) *metav1.PartialObjectMetadata {
	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetLabels(l)
	obj.SetGeneration(1)
	obj.SetResourceVersion("1")
	return obj
}

func tenantRequest(namespace string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: namespace,
	}}
}

func TestTenantOperandsCoverRenderedKinds(t *testing.T) {
	g := NewWithT(t)

	watched := map[schema.GroupVersionKind]bool{tenantreconcile.GVKNetworkPolicy: true}
	for _, op := range tenantOperands() {
		watched[op.gvk] = true
	}

	_, file, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	overlays := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "maas-api", "deploy", "overlays")
	for _, overlay := range []string{"odh", "xks"} {
		rendered, err := tenantreconcile.RenderKustomize(filepath.Join(overlays, overlay), operandInfraNS)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rendered).NotTo(BeEmpty())
		for i := range rendered {
			g.Expect(watched).To(HaveKey(rendered[i].GroupVersionKind()),
				"overlay %s renders %s %q with no operand watch", overlay, rendered[i].GroupVersionKind(), rendered[i].GetName())
		}
	}

	// Appended by PostRender (telemetry, autoscaling) and ensureUsageLogsEnvoyFilter.
	for _, gvk := range []schema.GroupVersionKind{
		tenantreconcile.GVKTelemetryPolicy,
		tenantreconcile.GVKIstioTelemetry,
		tenantreconcile.GVKHPA,
		tenantreconcile.GVKEnvoyFilter,
	} {
		g.Expect(watched).To(HaveKey(gvk))
	}
}

func operandTestCRD(name, version string, served, established bool) *extv1.CustomResourceDefinition {
	crd := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: extv1.CustomResourceDefinitionSpec{
			Versions: []extv1.CustomResourceDefinitionVersion{{Name: version, Served: served, Storage: true}},
		},
	}
	if established {
		crd.Status.Conditions = []extv1.CustomResourceDefinitionCondition{{Type: extv1.Established, Status: extv1.ConditionTrue}}
	}
	return crd
}

func TestTenantOperandCacheByObject(t *testing.T) {
	g := NewWithT(t)

	s := k8sruntime.NewScheme()
	g.Expect(extv1.AddToScheme(s)).To(Succeed())
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(
		operandTestCRD("destinationrules.networking.istio.io", "v1", true, true),
		operandTestCRD("envoyfilters.networking.istio.io", "v1alpha3", true, false),
		operandTestCRD("telemetries.telemetry.istio.io", "v1alpha1", true, true),
	).Build()

	byObject := TenantOperandCacheByObject(t.Context(), reader, operandInfraNS, operandGatewayNS)

	scoped := map[schema.GroupVersionKind]cache.ByObject{}
	for obj, cfg := range byObject {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		g.Expect(err).NotTo(HaveOccurred())
		scoped[gvk] = cfg
	}

	g.Expect(scoped).To(HaveLen(6), "Service, SA, CronJob, HPA, ClusterRole and the one served CRD kind")
	for _, gvk := range []schema.GroupVersionKind{
		tenantreconcile.GVKService,
		tenantreconcile.GVKServiceAccount,
		tenantreconcile.GVKCronJob,
		tenantreconcile.GVKHPA,
		tenantreconcile.GVKDestinationRule,
	} {
		g.Expect(scoped).To(HaveKey(gvk))
		g.Expect(scoped[gvk].Namespaces).To(HaveLen(2))
		g.Expect(scoped[gvk].Namespaces).To(HaveKey(operandInfraNS))
		g.Expect(scoped[gvk].Namespaces).To(HaveKey(operandGatewayNS))
	}

	g.Expect(scoped).To(HaveKey(gvkClusterRole))
	g.Expect(scoped[gvkClusterRole].Namespaces).To(BeNil())
	g.Expect(scoped[gvkClusterRole].Label.Matches(labels.Set(trackedBy("red", operandRedNS)))).To(BeTrue())
	g.Expect(scoped[gvkClusterRole].Label.Matches(labels.Set{"app": "other"})).To(BeFalse())

	g.Expect(scoped).NotTo(HaveKey(tenantreconcile.GVKEnvoyFilter), "CRD not established yet")
	g.Expect(scoped).NotTo(HaveKey(tenantreconcile.GVKIstioTelemetry), "watched version not served")
	g.Expect(scoped).NotTo(HaveKey(tenantreconcile.GVKCertificate), "CRD absent")

	// Informers shared with other controllers must keep their cluster-wide scope.
	for _, gvk := range []schema.GroupVersionKind{
		tenantreconcile.GVKDeployment,
		tenantreconcile.GVKConfigMap,
		tenantreconcile.GVKHTTPRoute,
		tenantreconcile.GVKTokenRateLimitPolicy,
	} {
		g.Expect(scoped).NotTo(HaveKey(gvk))
	}
}

// mapTenantOperandToMaasTenantConfig is the labelled-owner lookup; shared operands add a
// fallback on top of it (TestOperandMapperForSharedOperands).
func TestMapTenantOperandToMaasTenantConfig(t *testing.T) {
	tests := []struct {
		name      string
		discovery bool
		obj       client.Object
		want      []reconcile.Request
	}{
		{
			name:      "tracking namespace selects the tenant config",
			discovery: true,
			obj:       operandMeta(tenantreconcile.GVKServiceAccount, operandGatewayNS, tenantreconcile.PayloadProcessingServiceAccountName("red"), trackedBy("red", operandRedNS)),
			want:      []reconcile.Request{tenantRequest(operandRedNS)},
		},
		{
			name:      "cluster-scoped operand maps the same way",
			discovery: true,
			obj:       operandMeta(tenantreconcile.GVKClusterRoleBinding, "", tenantreconcile.PayloadProcessingReaderClusterRoleBindingNameForTenant("red"), trackedBy("red", operandRedNS)),
			want:      []reconcile.Request{tenantRequest(operandRedNS)},
		},
		{
			name:      "without discovery every operand belongs to the default tenant",
			discovery: false,
			obj:       operandMeta(tenantreconcile.GVKServiceAccount, operandGatewayNS, tenantreconcile.PayloadProcessingServiceAccountName("red"), trackedBy("red", operandRedNS)),
			want:      []reconcile.Request{tenantRequest(operandDefaultNS)},
		},
		{
			name:      "unlabelled object is not an operand",
			discovery: true,
			obj:       operandMeta(tenantreconcile.GVKServiceAccount, operandInfraNS, "maas-api", nil),
		},
		{
			name:      "externally owned maas-api Deployment maps to the default tenant",
			discovery: true,
			obj:       &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "maas-api", Namespace: operandInfraNS}},
			want:      []reconcile.Request{tenantRequest(operandDefaultNS)},
		},
		{
			name:      "unlabelled per-tenant maas-api Deployment is not guessed at",
			discovery: true,
			obj:       &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "maas-api-red", Namespace: operandInfraNS}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			r := operandTestReconciler(nil, tt.discovery)
			g.Expect(r.mapTenantOperandToMaasTenantConfig(t.Context(), tt.obj)).To(Equal(tt.want))
		})
	}
}

func TestOperandMapperForSharedOperands(t *testing.T) {
	now := metav1.Now()
	redConfig := func(mutate func(*maasv1alpha1.MaasTenantConfig)) *maasv1alpha1.MaasTenantConfig {
		tenant := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: operandRedNS,
		}}
		if mutate != nil {
			mutate(tenant)
		}
		return tenant
	}
	survivor := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: operandDefaultNS,
	}}
	var serviceAccounts, services tenantOperand
	for _, op := range tenantOperands() {
		switch op.gvk {
		case tenantreconcile.GVKServiceAccount:
			serviceAccounts = op
		case tenantreconcile.GVKService:
			services = op
		}
	}
	sharedSA := operandMeta(tenantreconcile.GVKServiceAccount, operandInfraNS, "maas-api", trackedBy("red", operandRedNS))
	perTenantService := operandMeta(tenantreconcile.GVKService, operandInfraNS, tenantreconcile.MaaSAPIServiceName("red"), trackedBy("red", operandRedNS))
	everyTenant := []reconcile.Request{tenantRequest(operandDefaultNS), tenantRequest(operandRedNS)}

	tests := []struct {
		name  string
		red   *maasv1alpha1.MaasTenantConfig
		op    tenantOperand
		obj   client.Object
		want  []reconcile.Request
		exact bool
	}{
		{name: "owner Managed", red: redConfig(nil), op: serviceAccounts, obj: sharedSA, want: []reconcile.Request{tenantRequest(operandRedNS)}, exact: true},
		{name: "owner gone", op: serviceAccounts, obj: sharedSA, want: []reconcile.Request{tenantRequest(operandDefaultNS)}},
		{name: "owner deleting", op: serviceAccounts, obj: sharedSA, want: everyTenant, red: redConfig(func(tc *maasv1alpha1.MaasTenantConfig) {
			tc.DeletionTimestamp = &now
			tc.Finalizers = []string{tenantFinalizer}
		})},
		{name: "owner Unmanaged", op: serviceAccounts, obj: sharedSA, want: everyTenant, red: redConfig(func(tc *maasv1alpha1.MaasTenantConfig) {
			tc.Annotations = map[string]string{managementStateAnnotation: managementStateUnmanaged}
		})},
		{name: "per-tenant operand of a gone owner", op: services, obj: perTenantService, want: []reconcile.Request{tenantRequest(operandRedNS)}, exact: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			objs := []client.Object{survivor.DeepCopy()}
			if tt.red != nil {
				objs = append(objs, tt.red)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			got := operandTestReconciler(c, true).operandMapper(tt.op)(t.Context(), tt.obj)
			if tt.exact {
				g.Expect(got).To(Equal(tt.want))
				return
			}
			g.Expect(got).To(ContainElements(tt.want))
		})
	}
}

func TestTenantOperandChanged(t *testing.T) {
	r := operandTestReconciler(nil, true)
	base := func() *metav1.PartialObjectMetadata {
		return operandMeta(tenantreconcile.GVKConfigMap, operandInfraNS, "maas-parameters", trackedBy("red", operandRedNS))
	}
	with := func(mutate func(*metav1.PartialObjectMetadata)) *metav1.PartialObjectMetadata {
		obj := base()
		mutate(obj)
		return obj
	}
	unmanaged := func(o *metav1.PartialObjectMetadata) {
		o.SetAnnotations(map[string]string{tenantreconcile.AnnotationManaged: "false"})
	}

	updates := []struct {
		name     string
		anyWrite bool
		old, new *metav1.PartialObjectMetadata
		want     bool
	}{
		{
			name: "spec change bumps generation",
			old:  base(),
			new:  with(func(o *metav1.PartialObjectMetadata) { o.SetGeneration(2); o.SetResourceVersion("2") }),
			want: true,
		},
		{
			name: "status write is ignored for kinds with generation",
			old:  base(),
			new:  with(func(o *metav1.PartialObjectMetadata) { o.SetResourceVersion("2") }),
			want: false,
		},
		{
			name:     "data write fires for kinds without generation",
			anyWrite: true,
			old:      base(),
			new:      with(func(o *metav1.PartialObjectMetadata) { o.SetResourceVersion("2") }),
			want:     true,
		},
		{
			name:     "resync with identical objects is ignored",
			anyWrite: true,
			old:      base(),
			new:      base(),
			want:     false,
		},
		{
			name:     "another tenant relabelling a shared operand is ignored",
			anyWrite: true,
			old:      base(),
			new: with(func(o *metav1.PartialObjectMetadata) {
				o.SetLabels(trackedBy("blue", "ai-tenant-blue"))
				o.SetGeneration(2)
				o.SetResourceVersion("2")
			}),
			want: false,
		},
		{
			name:     "stripping the tracking labels is drift",
			anyWrite: true,
			old:      base(),
			new: with(func(o *metav1.PartialObjectMetadata) {
				o.SetLabels(nil)
				o.SetResourceVersion("2")
			}),
			want: true,
		},
		{
			name: "other label change is drift",
			old:  base(),
			new: with(func(o *metav1.PartialObjectMetadata) {
				l := trackedBy("red", operandRedNS)
				l["app.kubernetes.io/part-of"] = "edited"
				o.SetLabels(l)
			}),
			want: true,
		},
		{
			name: "owner reference removal is drift",
			old: with(func(o *metav1.PartialObjectMetadata) {
				o.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Config", Name: "default", UID: "cfg"}})
			}),
			new:  base(),
			want: true,
		},
		{
			name: "opting back into management fires",
			old:  with(unmanaged),
			new:  base(),
			want: true,
		},
		{
			name:     "edits while unmanaged are ignored",
			anyWrite: true,
			old:      with(unmanaged),
			new: with(func(o *metav1.PartialObjectMetadata) {
				unmanaged(o)
				o.SetResourceVersion("2")
			}),
			want: false,
		},
		{
			name: "spec change while unmanaged fires for readiness checks",
			old:  with(unmanaged),
			new: with(func(o *metav1.PartialObjectMetadata) {
				unmanaged(o)
				o.SetGeneration(2)
				o.SetResourceVersion("2")
			}),
			want: true,
		},
		{
			name:     "object outside the platform namespaces is ignored",
			anyWrite: true,
			old:      with(func(o *metav1.PartialObjectMetadata) { o.SetNamespace("elsewhere") }),
			new: with(func(o *metav1.PartialObjectMetadata) {
				o.SetNamespace("elsewhere")
				o.SetResourceVersion("2")
			}),
			want: false,
		},
	}
	for _, tt := range updates {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got := r.tenantOperandChanged(tenantOperand{anyWrite: tt.anyWrite}).Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new})
			g.Expect(got).To(Equal(tt.want))
		})
	}

	t.Run("create and delete of operands fire", func(t *testing.T) {
		g := NewWithT(t)
		p := r.tenantOperandChanged(tenantOperand{})
		g.Expect(p.Create(event.CreateEvent{Object: base()})).To(BeTrue())
		g.Expect(p.Delete(event.DeleteEvent{Object: base()})).To(BeTrue())
		g.Expect(p.Create(event.CreateEvent{Object: operandMeta(gvkClusterRole, "", "maas-api", trackedBy("red", operandRedNS))})).To(BeTrue())
	})

	t.Run("create of unlabelled or foreign objects is ignored", func(t *testing.T) {
		g := NewWithT(t)
		p := r.tenantOperandChanged(tenantOperand{})
		g.Expect(p.Create(event.CreateEvent{Object: with(func(o *metav1.PartialObjectMetadata) { o.SetLabels(nil) })})).To(BeFalse())
		g.Expect(p.Create(event.CreateEvent{Object: with(func(o *metav1.PartialObjectMetadata) { o.SetNamespace("elsewhere") })})).To(BeFalse())
	})
}

func TestMaaSAPIRolloutChanged(t *testing.T) {
	r := operandTestReconciler(nil, true)
	deployment := func(name, namespace string, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 2},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 2,
				UpdatedReplicas:    1,
				AvailableReplicas:  available,
			},
		}
	}

	tests := []struct {
		name     string
		old, new *appsv1.Deployment
		want     bool
	}{
		{
			name: "maas-api becomes available",
			old:  deployment("maas-api", operandInfraNS, 0),
			new:  deployment("maas-api", operandInfraNS, 1),
			want: true,
		},
		{
			name: "per-tenant maas-api loses availability",
			old:  deployment("maas-api-red", operandInfraNS, 1),
			new:  deployment("maas-api-red", operandInfraNS, 0),
			want: true,
		},
		{
			name: "status churn without crossing the gate",
			old:  deployment("maas-api", operandInfraNS, 1),
			new: func() *appsv1.Deployment {
				d := deployment("maas-api", operandInfraNS, 1)
				d.Status.ReadyReplicas = 1
				return d
			}(),
			want: false,
		},
		{
			name: "payload-processing rollout does not gate tenant readiness",
			old:  deployment("payload-processing", operandInfraNS, 0),
			new:  deployment("payload-processing", operandInfraNS, 1),
			want: false,
		},
		{
			name: "maas-api outside the app namespace",
			old:  deployment("maas-api", operandGatewayNS, 0),
			new:  deployment("maas-api", operandGatewayNS, 1),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(r.maasAPIRolloutChanged().Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new})).To(Equal(tt.want))
		})
	}
}

func TestGatewayPredicates(t *testing.T) {
	g := NewWithT(t)
	r := operandTestReconciler(nil, true)
	gateway := func(namespace string) *gwapiv1.Gateway {
		return &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: operandGateway, Namespace: namespace, Generation: 1}}
	}

	g.Expect(r.inGatewayNamespace(gateway(operandGatewayNS))).To(BeTrue())
	g.Expect(r.inGatewayNamespace(gateway("elsewhere"))).To(BeFalse())

	// The reconcile reads a Gateway only for existence.
	p := createOrDeleteOnly()
	updated := gateway(operandGatewayNS)
	updated.Generation = 2
	updated.Status.Conditions = []metav1.Condition{{Type: string(gwapiv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue}}
	g.Expect(p.Create(event.CreateEvent{Object: gateway(operandGatewayNS)})).To(BeTrue())
	g.Expect(p.Delete(event.DeleteEvent{Object: gateway(operandGatewayNS)})).To(BeTrue())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: gateway(operandGatewayNS), ObjectNew: updated})).To(BeFalse())
}

// gatewayTenantFixtures returns an unlabelled default tenant config on the fallback
// gateway and an AITenant-managed tenant whose gateway comes from AITenant status.
func gatewayTenantFixtures() []client.Object {
	return []client.Object{
		&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: operandDefaultNS,
		}},
		&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: operandRedNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "red",
			},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      "red",
				tenantreconcile.AnnotationAITenantNamespace: tenantreconcile.DefaultAITenantNamespace,
			},
		}},
		&maasv1alpha1.AITenant{
			ObjectMeta: metav1.ObjectMeta{Name: "red", Namespace: tenantreconcile.DefaultAITenantNamespace},
			Status: maasv1alpha1.AITenantStatus{
				GatewayRef: maasv1alpha1.TenantGatewayRef{Name: "red-gateway", Namespace: operandGatewayNS},
			},
		},
	}
}

func TestMapGatewayToMaasTenantConfigs(t *testing.T) {
	tests := []struct {
		name      string
		discovery bool
		gateway   string
		want      []reconcile.Request
	}{
		{name: "fallback gateway selects the default tenant", discovery: true, gateway: operandGateway, want: []reconcile.Request{tenantRequest(operandDefaultNS)}},
		{name: "AITenant gateway selects its tenant", discovery: true, gateway: "red-gateway", want: []reconcile.Request{tenantRequest(operandRedNS)}},
		{name: "unreferenced gateway selects nothing", discovery: true, gateway: "other"},
		{name: "without discovery only the default tenant is reconciled", discovery: false, gateway: "red-gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayTenantFixtures()...).Build()
			r := operandTestReconciler(c, tt.discovery)
			gw := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: tt.gateway, Namespace: operandGatewayNS}}
			g.Expect(r.mapGatewayToMaasTenantConfigs(t.Context(), gw)).To(Equal(tt.want))
		})
	}
}

func TestMapGatewayAuthPolicyToMaasTenantConfigs(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayTenantFixtures()...).Build()
	r := operandTestReconciler(c, true)

	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(tenantreconcile.GVKAuthPolicy)
	policy.SetNamespace(operandGatewayNS)
	policy.SetName("red-gateway-maas-auth")
	policy.SetLabels(map[string]string{"app.kubernetes.io/part-of": gatewayAuthPolicyPartOf})
	g.Expect(unstructured.SetNestedField(policy.Object, "red-gateway", "spec", "targetRef", "name")).To(Succeed())

	g.Expect(r.isGatewayAuthPolicy(policy)).To(BeTrue())
	g.Expect(r.mapGatewayAuthPolicyToMaasTenantConfigs(t.Context(), policy)).To(Equal([]reconcile.Request{tenantRequest(operandRedNS)}))

	modelPolicy := policy.DeepCopy()
	modelPolicy.SetLabels(map[string]string{"app.kubernetes.io/part-of": "maas"})
	g.Expect(r.isGatewayAuthPolicy(modelPolicy)).To(BeFalse())
}

func TestMapDeletedTenantChildToMaasTenantConfig(t *testing.T) {
	now := metav1.NewTime(time.Now())
	deleting := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name:              maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace:         operandRedNS,
		DeletionTimestamp: &now,
		Finalizers:        []string{tenantFinalizer},
	}}
	live := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: operandDefaultNS,
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deleting, live).Build()
	r := operandTestReconciler(c, true)

	subscriptionIn := func(ns string) client.Object {
		return &maasv1alpha1.MaaSSubscription{ObjectMeta: metav1.ObjectMeta{Name: "sub", Namespace: ns}}
	}

	g := NewWithT(t)
	g.Expect(r.mapDeletedTenantChildToMaasTenantConfig(t.Context(), subscriptionIn(operandRedNS))).
		To(Equal([]reconcile.Request{tenantRequest(operandRedNS)}))
	g.Expect(r.mapDeletedTenantChildToMaasTenantConfig(t.Context(), subscriptionIn(operandDefaultNS))).To(BeEmpty(),
		"a tenant config that is not deleting does not wait on its MaaS CRs")
	g.Expect(r.mapDeletedTenantChildToMaasTenantConfig(t.Context(), subscriptionIn("no-tenant"))).To(BeEmpty())

	failing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deleting).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("cache not synced")
		},
	}).Build()
	g.Expect(operandTestReconciler(failing, true).mapDeletedTenantChildToMaasTenantConfig(t.Context(), subscriptionIn(operandRedNS))).To(BeEmpty())
}

func TestEnqueueAllTenants(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayTenantFixtures()...).Build()

	g := NewWithT(t)
	g.Expect(operandTestReconciler(c, true).enqueueAllTenants(t.Context(), nil)).
		To(ConsistOf(tenantRequest(operandDefaultNS), tenantRequest(operandRedNS)))
	g.Expect(operandTestReconciler(c, false).enqueueAllTenants(t.Context(), nil)).
		To(Equal([]reconcile.Request{tenantRequest(operandDefaultNS)}))
}

func operandReconcileScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	for _, add := range []func(*k8sruntime.Scheme) error{
		clientgoscheme.AddToScheme, gwapiv1.Install, maasv1alpha1.AddToScheme, extv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func defaultTenantConfig() *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: operandDefaultNS,
	}}
}

type waitingTenant struct {
	tenant       *maasv1alpha1.MaasTenantConfig
	restMapper   apimeta.RESTMapper
	manifestPath string
	objs         []client.Object
	interceptors interceptor.Funcs
	logs         *[]string
}

// reconcileWaitingTenant runs one reconcile of a tenant config with a live Config
// anchor, so it gets past the Config gate into the gateway, dependency, prerequisite
// and platform gates.
func reconcileWaitingTenant(t *testing.T, w waitingTenant) (ctrl.Result, *maasv1alpha1.MaasTenantConfig) {
	t.Helper()
	res, tenant, err := reconcileTenantOnce(t, w)
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	return res, tenant
}

func reconcileTenantOnce(t *testing.T, w waitingTenant) (ctrl.Result, *maasv1alpha1.MaasTenantConfig, error) {
	t.Helper()
	g := NewWithT(t)

	if w.tenant == nil {
		w.tenant = defaultTenantConfig()
	}
	if w.manifestPath == "" {
		w.manifestPath = "/unused"
	}
	s := operandReconcileScheme(t)
	objs := append([]client.Object{w.tenant, &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.ConfigInstanceName,
		UID:  types.UID("config-uid"),
	}}}, w.objs...)
	b := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
		WithObjects(objs...)
	if w.restMapper != nil {
		b = b.WithRESTMapper(w.restMapper)
	}
	c := b.WithInterceptorFuncs(w.interceptors).Build()
	r := operandTestReconciler(c, false)
	r.Scheme = s
	r.ManifestPath = w.manifestPath

	ctx := t.Context()
	if w.logs != nil {
		ctx = log.IntoContext(ctx, funcr.New(func(prefix, args string) {
			*w.logs = append(*w.logs, prefix+" "+args)
		}, funcr.Options{Verbosity: 1}))
	}
	key := client.ObjectKeyFromObject(w.tenant)
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})

	var updated maasv1alpha1.MaasTenantConfig
	g.Expect(c.Get(t.Context(), key, &updated)).To(Succeed())
	return res, &updated, err
}

// readErrorRESTMapper fails lookups of one kind the way an unreachable discovery
// endpoint does, as opposed to the NoMatch of a kind that is not installed.
type readErrorRESTMapper struct {
	apimeta.RESTMapper
	gvk schema.GroupVersionKind
}

func (m readErrorRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*apimeta.RESTMapping, error) {
	if gk == m.gvk.GroupKind() {
		return nil, errors.New("discovery unavailable")
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

func TestTenantReconcile_ReadErrorsRetryInsteadOfWaiting(t *testing.T) {
	gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: operandGateway, Namespace: operandGatewayNS}}
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: operandInfraNS},
		Data:       map[string][]byte{tenantreconcile.MaaSDBSecretKey: []byte("postgresql://maas@db.example.com:5432/maas")},
	}
	unavailable := errors.New("apiserver unavailable")
	failGet := func(match func(client.ObjectKey, client.Object) bool) interceptor.Funcs {
		return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if match(key, obj) {
				return unavailable
			}
			return c.Get(ctx, key, obj, opts...)
		}}
	}
	failList := func(kind string) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == kind+"List" {
				return unavailable
			}
			return c.List(ctx, list, opts...)
		}}
	}
	isGateway := func(_ client.ObjectKey, obj client.Object) bool { _, ok := obj.(*gwapiv1.Gateway); return ok }
	isDBSecret := func(key client.ObjectKey, obj client.Object) bool {
		_, ok := obj.(*corev1.Secret)
		return ok && key.Name == tenantreconcile.MaaSDBSecretName
	}
	withAuthConfig := restMapperWith(t, tenantreconcile.GVKAuthConfig)
	withAuthorino := restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKAuthorino)

	tests := []struct {
		name       string
		w          waitingTenant
		wantReason string
	}{
		{
			name:       "gateway read fails",
			w:          waitingTenant{interceptors: failGet(isGateway)},
			wantReason: "GatewayCheckFailed",
		},
		{
			name: "dependency discovery fails",
			w: waitingTenant{
				objs:       []client.Object{gateway},
				restMapper: readErrorRESTMapper{RESTMapper: withAuthConfig, gvk: tenantreconcile.GVKAuthConfig},
			},
			wantReason: "DependencyCheckFailed",
		},
		{
			name:       "database Secret read fails",
			w:          waitingTenant{objs: []client.Object{gateway}, restMapper: withAuthConfig, interceptors: failGet(isDBSecret)},
			wantReason: "PrerequisiteCheckFailed",
		},
		{
			name: "Authorino discovery fails",
			w: waitingTenant{
				objs:       []client.Object{gateway, dbSecret},
				restMapper: readErrorRESTMapper{RESTMapper: withAuthorino, gvk: tenantreconcile.GVKAuthorino},
			},
			wantReason: "PrerequisiteCheckFailed",
		},
		{
			name:       "Authorino list fails",
			w:          waitingTenant{objs: []client.Object{gateway, dbSecret}, restMapper: withAuthorino, interceptors: failList(tenantreconcile.GVKAuthorino.Kind)},
			wantReason: "PrerequisiteCheckFailed",
		},
		{
			name:       "DSCInitialization list fails",
			w:          waitingTenant{objs: []client.Object{gateway, dbSecret}, restMapper: withAuthConfig, interceptors: failList("DSCInitialization")},
			wantReason: "PrerequisiteCheckFailed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			// Absent and could-not-check look alike to the caller unless the error surfaces;
			// only the error path retries, with backoff.
			_, tenant, err := reconcileTenantOnce(t, tt.w)
			g.Expect(err).To(HaveOccurred())
			g.Expect(readyReason(tenant)).To(Equal(tt.wantReason))
		})
	}
}

func readyReason(tenant *maasv1alpha1.MaasTenantConfig) string {
	if c := apimeta.FindStatusCondition(tenant.Status.Conditions, tenantreconcile.ReadyConditionType); c != nil {
		return c.Reason
	}
	return ""
}

func restMapperWith(t *testing.T, gvks ...schema.GroupVersionKind) apimeta.RESTMapper {
	t.Helper()
	extra := apimeta.NewDefaultRESTMapper(nil)
	for _, gvk := range gvks {
		extra.Add(gvk, apimeta.RESTScopeNamespace)
	}
	return apimeta.MultiRESTMapper{testrestmapper.TestOnlyStaticRESTMapper(operandReconcileScheme(t)), testRESTMapper(), extra}
}

func TestTenantReconcile_WaitsOnWatchesInsteadOfPolling(t *testing.T) {
	gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: operandGateway, Namespace: operandGatewayNS}}
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: operandInfraNS},
		Data:       map[string][]byte{tenantreconcile.MaaSDBSecretKey: []byte("postgresql://maas@postgres." + operandInfraNS + ".svc:5432/maas")},
	}
	kuadrantFilter := &unstructured.Unstructured{}
	kuadrantFilter.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	kuadrantFilter.SetNamespace(operandGatewayNS)
	kuadrantFilter.SetName("kuadrant-" + operandGateway)
	GVKWasmPlugin := schema.GroupVersionKind{Group: "extensions.istio.io", Version: "v1alpha1", Kind: "WasmPlugin"}
	kuadrantPlugin := &unstructured.Unstructured{}
	kuadrantPlugin.SetGroupVersionKind(GVKWasmPlugin)
	kuadrantPlugin.SetNamespace(operandGatewayNS)
	kuadrantPlugin.SetName("kuadrant-" + operandGateway)

	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	odhOverlay := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "maas-api", "deploy", "overlays", "odh")
	// A peer claim on the payload-processing handshake makes RunPlatform return
	// DeploymentPending after the Kuadrant detection and before any apply.
	awaitingPeer := defaultTenantConfig()
	awaitingPeer.Annotations = map[string]string{tenantreconcile.AnnotationPayloadProcessingStatus: "praxis-owned"}

	tests := []struct {
		name        string
		w           waitingTenant
		wantReason  string
		wantRequeue time.Duration
	}{
		{
			name: "platform context unresolved",
			w: waitingTenant{tenant: &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name:      maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace: operandDefaultNS,
				Labels: map[string]string{
					tenantreconcile.LabelManagedByAITenant: "true",
					tenantreconcile.LabelTenantName:        "red",
					tenantreconcile.LabelTenantNamespace:   operandDefaultNS,
				},
			}}},
			wantReason: "InvalidGateway",
		},
		{
			name:       "missing gateway",
			wantReason: "GatewayNotReady",
		},
		{
			name:       "missing dependency CRD",
			w:          waitingTenant{objs: []client.Object{gateway}},
			wantReason: "DependenciesNotAvailable",
		},
		{
			name: "dependency CRD established before discovery serves it",
			w: waitingTenant{objs: []client.Object{gateway, &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.Dependencies[0].CRD},
				Spec: extv1.CustomResourceDefinitionSpec{
					Group: tenantreconcile.GVKAuthConfig.Group,
					Names: extv1.CustomResourceDefinitionNames{Kind: tenantreconcile.GVKAuthConfig.Kind},
					Versions: []extv1.CustomResourceDefinitionVersion{
						{Name: tenantreconcile.GVKAuthConfig.Version, Served: true, Storage: true},
					},
				},
				Status: extv1.CustomResourceDefinitionStatus{Conditions: []extv1.CustomResourceDefinitionCondition{
					{Type: extv1.Established, Status: extv1.ConditionTrue},
				}},
			}}},
			wantReason:  "DependenciesNotAvailable",
			wantRequeue: discoveryLagRetry,
		},
		{
			name:       "missing database Secret",
			w:          waitingTenant{restMapper: restMapperWith(t, tenantreconcile.GVKAuthConfig), objs: []client.Object{gateway}},
			wantReason: "PrerequisitesNotMet",
		},
		{
			name: "deployment pending with Kuadrant wasm detected",
			w: waitingTenant{
				tenant:       awaitingPeer,
				manifestPath: odhOverlay,
				restMapper:   restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKEnvoyFilter, GVKWasmPlugin),
				objs:         []client.Object{gateway, dbSecret, kuadrantFilter},
			},
			wantReason: "DeploymentsNotReady",
		},
		{
			// A WasmPlugin cannot be watched; the probe re-detects it without a re-render.
			name: "deployment pending with Kuadrant wasm from a WasmPlugin leaves it to the probe",
			w: waitingTenant{
				tenant:       awaitingPeer,
				manifestPath: odhOverlay,
				restMapper:   restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKEnvoyFilter, GVKWasmPlugin),
				objs:         []client.Object{gateway, dbSecret, kuadrantPlugin},
			},
			wantReason: "DeploymentsNotReady",
		},
		{
			name: "deployment pending on the router fallback rechecks Kuadrant",
			w: waitingTenant{
				tenant:       awaitingPeer,
				manifestPath: odhOverlay,
				restMapper:   restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKEnvoyFilter, GVKWasmPlugin),
				objs:         []client.Object{gateway, dbSecret},
			},
			wantReason:  "DeploymentsNotReady",
			wantRequeue: kuadrantRecheckInterval,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			tt.w.tenant = tt.w.tenant.DeepCopy()
			res, tenant := reconcileWaitingTenant(t, tt.w)
			g.Expect(readyReason(tenant)).To(Equal(tt.wantReason))
			g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: tt.wantRequeue}))
		})
	}
}

func TestTenantReconcile_SuccessRequeuesOnlyForKuadrantDetection(t *testing.T) {
	tests := []struct {
		name   string
		runRes *tenantreconcile.RunResult
		want   ctrl.Result
	}{
		{name: "Kuadrant wasm carried by the watched EnvoyFilter", runRes: &tenantreconcile.RunResult{}, want: ctrl.Result{}},
		{name: "router fallback", runRes: &tenantreconcile.RunResult{KuadrantRouterFallback: true}, want: ctrl.Result{RequeueAfter: kuadrantRecheckInterval}},
		// Kuadrant anchors stay in place when the WasmPlugin cannot be read.
		{name: "detection unverified", runRes: &tenantreconcile.RunResult{KuadrantDetectionWarning: "cannot get WasmPlugin"}, want: ctrl.Result{RequeueAfter: 5 * time.Minute}},
		{name: "Kuadrant wasm carried by a WasmPlugin", runRes: &tenantreconcile.RunResult{KuadrantWasmPlugin: true}, want: ctrl.Result{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			tenant := defaultTenantConfig()
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
				WithObjects(tenant).
				Build()
			r := operandTestReconciler(c, false)

			res, err := r.setFinalStatus(t.Context(), tenant, tt.runRes)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res).To(Equal(tt.want))
			g.Expect(tenant.Status.Phase).To(Equal("Active"))
		})
	}
}

func TestKuadrantGatewayFilterMapping(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayTenantFixtures()...).Build()
	r := operandTestReconciler(c, true)

	filter := func(namespace, name string) client.Object {
		return operandMeta(tenantreconcile.GVKEnvoyFilter, namespace, name, nil)
	}

	g.Expect(r.isKuadrantGatewayFilter(filter(operandGatewayNS, "kuadrant-red-gateway"))).To(BeTrue())
	g.Expect(r.isKuadrantGatewayFilter(filter(operandGatewayNS, "payload-processing"))).To(BeFalse())
	g.Expect(r.isKuadrantGatewayFilter(filter(operandGatewayNS, "kuadrant-"))).To(BeFalse())
	g.Expect(r.isKuadrantGatewayFilter(filter("elsewhere", "kuadrant-red-gateway"))).To(BeFalse())

	g.Expect(r.mapKuadrantGatewayFilterToMaasTenantConfigs(t.Context(), filter(operandGatewayNS, "kuadrant-red-gateway"))).
		To(Equal([]reconcile.Request{tenantRequest(operandRedNS)}))
	g.Expect(r.mapKuadrantGatewayFilterToMaasTenantConfigs(t.Context(), filter(operandGatewayNS, "kuadrant-"+operandGateway))).
		To(Equal([]reconcile.Request{tenantRequest(operandDefaultNS)}))
}

func TestHPASpecDrift(t *testing.T) {
	r := operandTestReconciler(nil, true)
	var hpaOperand tenantOperand
	for _, op := range tenantOperands() {
		if op.gvk == tenantreconcile.GVKHPA {
			hpaOperand = op
		}
	}
	hpa := func(maxReplicas int32, currentReplicas int32) *autov2.HorizontalPodAutoscaler {
		return &autov2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name: "payload-processing", Namespace: operandGatewayNS,
				Labels: trackedBy("red", operandRedNS), ResourceVersion: "1",
			},
			Spec:   autov2.HorizontalPodAutoscalerSpec{MaxReplicas: maxReplicas},
			Status: autov2.HorizontalPodAutoscalerStatus{CurrentReplicas: currentReplicas},
		}
	}

	g := NewWithT(t)
	p := r.tenantOperandChanged(hpaOperand)
	statusOnly := hpa(10, 3)
	statusOnly.ResourceVersion = "2"
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: hpa(10, 1), ObjectNew: statusOnly})).To(BeFalse(), "metrics churn")
	specEdit := hpa(20, 1)
	specEdit.ResourceVersion = "2"
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: hpa(10, 1), ObjectNew: specEdit})).To(BeTrue(), "spec edit with generation still 0")
}

func TestScaledByHPA(t *testing.T) {
	deployment := func(replicas int32, image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "payload-processing", Namespace: operandGatewayNS,
				Labels: trackedBy("red", operandRedNS), Generation: int64(replicas),
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(replicas),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "pp", Image: image}}}},
			},
		}
	}
	hpa := &autov2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "payload-processing", Namespace: operandGatewayNS},
		Spec: autov2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autov2.CrossVersionObjectReference{Kind: "Deployment", Name: "payload-processing"},
		},
	}
	var deploymentOperand tenantOperand
	for _, op := range tenantOperands() {
		if op.gvk == tenantreconcile.GVKDeployment {
			deploymentOperand = op
		}
	}

	tests := []struct {
		name     string
		objs     []client.Object
		old, new *appsv1.Deployment
		want     bool
	}{
		{name: "HPA scales the Deployment", objs: []client.Object{hpa}, old: deployment(1, "a"), new: deployment(3, "a"), want: false},
		{name: "manual scale without an HPA is drift", old: deployment(1, "a"), new: deployment(3, "a"), want: true},
		{name: "scale together with a spec edit is drift", objs: []client.Object{hpa}, old: deployment(1, "a"), new: deployment(3, "b"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objs...).Build()
			r := operandTestReconciler(c, true)
			g.Expect(r.tenantOperandChanged(deploymentOperand).Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new})).To(Equal(tt.want))
		})
	}
}

func TestCRDServesVersion(t *testing.T) {
	g := NewWithT(t)
	g.Expect(crdServesVersion(nil, "v1")).To(BeFalse())
	g.Expect(crdServesVersion(operandTestCRD("a.example.com", "v1", true, false), "v1")).To(BeFalse(), "not established")
	g.Expect(crdServesVersion(operandTestCRD("a.example.com", "v1", true, true), "v1")).To(BeTrue())
	g.Expect(crdServesVersion(operandTestCRD("a.example.com", "v1alpha1", true, true), "v1")).To(BeFalse(), "other version")
	g.Expect(crdServesVersion(operandTestCRD("a.example.com", "v1", false, true), "v1")).To(BeFalse(), "version not served")
}

func TestTenantPlatformCRDEventsWakeTenants(t *testing.T) {
	crd := func(name string) *extv1.CustomResourceDefinition {
		return &extv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	p := crdNamed(tenantPlatformCRDs()...)

	for _, name := range []string{
		tenantreconcile.Dependencies[0].CRD,
		// Optional operand kinds: a tenant that rendered without them only warned.
		"envoyfilters.networking.istio.io",
		"telemetrypolicies.extensions.kuadrant.io",
		"certificates.cert-manager.io",
		"servicemonitors.monitoring.coreos.com",
		"authpolicies.kuadrant.io",
		"authorinos.operator.authorino.kuadrant.io",
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(p.Create(event.CreateEvent{Object: crd(name)})).To(BeTrue())
			established := crd(name)
			established.Status.Conditions = []extv1.CustomResourceDefinitionCondition{{Type: extv1.Established, Status: extv1.ConditionTrue}}
			g.Expect(p.Update(event.UpdateEvent{ObjectOld: crd(name), ObjectNew: established})).To(BeTrue())
		})
	}

	g := NewWithT(t)
	g.Expect(p.Create(event.CreateEvent{Object: crd("widgets.example.com")})).To(BeFalse())
}

// A settled Praxis tenant renders no IPP operands and runs no Kuadrant detection, so its
// usage-logs EnvoyFilter is the only EnvoyFilter it applies.
func TestTenantReconcile_OptionalKindAwaitingDiscovery(t *testing.T) {
	envoyFilterCRD := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "envoyfilters.networking.istio.io"},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group:    tenantreconcile.GVKEnvoyFilter.Group,
			Names:    extv1.CustomResourceDefinitionNames{Kind: tenantreconcile.GVKEnvoyFilter.Kind, Plural: "envoyfilters"},
			Scope:    extv1.NamespaceScoped,
			Versions: []extv1.CustomResourceDefinitionVersion{{Name: tenantreconcile.GVKEnvoyFilter.Version, Served: true, Storage: true}},
		},
		Status: extv1.CustomResourceDefinitionStatus{Conditions: []extv1.CustomResourceDefinitionCondition{
			{Type: extv1.Established, Status: extv1.ConditionTrue},
		}},
	}

	tests := []struct {
		name        string
		objs        []client.Object
		wantRequeue time.Duration
	}{
		{
			// The CRD is Established but discovery does not list EnvoyFilter yet; nothing
			// reports discovery catching up, so the reconcile has to come back.
			name:        "CRD established before discovery serves the kind",
			objs:        []client.Object{envoyFilterCRD},
			wantRequeue: discoveryLagRetry,
		},
		{
			// The CRD watch wakes the tenant when the CRD is installed.
			name: "CRD not installed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			s := operandReconcileScheme(t)
			praxis := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
				Name:      maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace: operandDefaultNS,
				Labels: map[string]string{
					tenantreconcile.LabelManagedByAITenant: "true",
					tenantreconcile.LabelTenantName:        "red",
					tenantreconcile.LabelTenantNamespace:   operandDefaultNS,
				},
				Annotations: map[string]string{
					tenantreconcile.AnnotationAITenantName:            "red",
					tenantreconcile.AnnotationAITenantNamespace:       tenantreconcile.DefaultAITenantNamespace,
					tenantreconcile.AnnotationPayloadProcessingType:   tenantreconcile.PayloadProcessingTypePraxis,
					tenantreconcile.AnnotationPayloadProcessingStatus: tenantreconcile.PayloadProcessingStatusCleanupComplete,
				},
			}}
			objs := append([]client.Object{
				praxis,
				&maasv1alpha1.Config{
					ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("config-uid")},
					Spec:       maasv1alpha1.ConfigSpec{UsageLogging: ptr.To(true)},
				},
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{Name: "red", Namespace: tenantreconcile.DefaultAITenantNamespace},
					Status: maasv1alpha1.AITenantStatus{
						GatewayRef: maasv1alpha1.TenantGatewayRef{Name: operandGateway, Namespace: operandGatewayNS},
					},
				},
				&gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: operandGateway, Namespace: operandGatewayNS}},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: operandInfraNS},
					Data:       map[string][]byte{tenantreconcile.MaaSDBSecretKey: []byte("postgresql://maas@db.example.com:5432/maas")},
				},
			}, tt.objs...)

			// The fake client stores any kind; a real client fails requests for kinds its REST
			// mapper does not know. No deployment controller runs either, so the rendered
			// maas-api Deployment reads back rolled out.
			unmapped := func(c client.WithWatch, obj client.Object) error {
				u, ok := obj.(*unstructured.Unstructured)
				if !ok {
					return nil
				}
				gvk := u.GroupVersionKind()
				_, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
				if apimeta.IsNoMatchError(err) {
					return err
				}
				return nil
			}
			clusterLikeClient := interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := unmapped(c, obj); err != nil {
						return err
					}
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if dep, ok := obj.(*appsv1.Deployment); ok {
						replicas := ptr.Deref(dep.Spec.Replicas, 1)
						dep.Status = appsv1.DeploymentStatus{
							ObservedGeneration: dep.Generation, Replicas: replicas, UpdatedReplicas: replicas, AvailableReplicas: replicas,
						}
					}
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := unmapped(c, obj); err != nil {
						return err
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(s).
				WithRESTMapper(restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKDestinationRule, gvkServiceMonitor)).
				WithStatusSubresource(&maasv1alpha1.MaasTenantConfig{}).
				WithObjects(objs...).
				WithInterceptorFuncs(clusterLikeClient).
				Build()
			r := operandTestReconciler(c, false)
			r.Scheme = s
			r.ManifestPath = filepath.Join(testUsageLogsManifestPath(t), "..", "..", "..", "..", "maas-api", "deploy", "overlays", "odh")
			r.MonitoringNamespace = usageLogsTestMonitoringNS
			r.UsageLogsManifestPath = testUsageLogsManifestPath(t)

			res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(praxis)})
			g.Expect(err).NotTo(HaveOccurred())

			var updated maasv1alpha1.MaasTenantConfig
			g.Expect(c.Get(t.Context(), client.ObjectKeyFromObject(praxis), &updated)).To(Succeed())
			degraded := apimeta.FindStatusCondition(updated.Status.Conditions, tenantreconcile.ConditionTypeDegraded)
			g.Expect(degraded).NotTo(BeNil())
			g.Expect(degraded.Message).To(ContainSubstring("Usage-logs EnvoyFilter not deployed"))
			g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: tt.wantRequeue}))
		})
	}
}

func TestTenantReconcile_LogsTimerWaits(t *testing.T) {
	g := NewWithT(t)
	gateway := &gwapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: operandGateway, Namespace: operandGatewayNS}}
	dbSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.MaaSDBSecretName, Namespace: operandInfraNS},
		Data:       map[string][]byte{tenantreconcile.MaaSDBSecretKey: []byte("postgresql://maas@db.example.com:5432/maas")},
	}
	_, file, _, ok := goruntime.Caller(0)
	g.Expect(ok).To(BeTrue())
	awaitingPeer := defaultTenantConfig()
	awaitingPeer.Annotations = map[string]string{tenantreconcile.AnnotationPayloadProcessingStatus: "praxis-owned"}
	GVKWasmPlugin := schema.GroupVersionKind{Group: "extensions.istio.io", Version: "v1alpha1", Kind: "WasmPlugin"}

	// A tenant on the router fallback waits on a timer; nothing but the log says so.
	var logs []string
	res, _ := reconcileWaitingTenant(t, waitingTenant{
		tenant:       awaitingPeer,
		manifestPath: filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "maas-api", "deploy", "overlays", "odh"),
		restMapper:   restMapperWith(t, tenantreconcile.GVKAuthConfig, tenantreconcile.GVKEnvoyFilter, GVKWasmPlugin),
		objs:         []client.Object{gateway, dbSecret},
		logs:         &logs,
	})

	g.Expect(res.RequeueAfter).To(Equal(kuadrantRecheckInterval))
	g.Expect(logs).To(ContainElement(And(
		ContainSubstring(`"reason"="Kuadrant wasm auth not found`),
		ContainSubstring(`"requeueAfter"="2m0s"`),
	)))
}

func TestKuadrantWasmPluginProbe(t *testing.T) {
	g := NewWithT(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gatewayTenantFixtures()...).Build()
	r := operandTestReconciler(c, true)
	probe := newKuadrantWasmPluginProbe(r, time.Minute)
	defaultGateway := types.NamespacedName{Namespace: operandGatewayNS, Name: operandGateway}
	redGateway := types.NamespacedName{Namespace: operandGatewayNS, Name: "red-gateway"}
	plugin := func(gateway types.NamespacedName) *unstructured.Unstructured {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(tenantreconcile.GVKWasmPlugin)
		p.SetNamespace(gateway.Namespace)
		p.SetName(tenantreconcile.KuadrantGatewayResourceName(gateway.Name))
		return p
	}

	// The default tenant rendered on the router fallback; red rendered against its plugin.
	probe.observe(defaultGateway, &tenantreconcile.RunResult{KuadrantRouterFallback: true})
	g.Expect(c.Create(t.Context(), plugin(redGateway))).To(Succeed())
	probe.observe(redGateway, &tenantreconcile.RunResult{KuadrantWasmPlugin: true})

	g.Expect(probe.changed(t.Context())).To(BeEmpty(), "nothing changed since the tenants rendered")

	g.Expect(c.Create(t.Context(), plugin(defaultGateway))).To(Succeed())
	g.Expect(c.Delete(t.Context(), plugin(redGateway))).To(Succeed())
	g.Expect(probe.changed(t.Context())).To(ConsistOf(tenantRequest(operandDefaultNS), tenantRequest(operandRedNS)))

	g.Expect(probe.changed(t.Context())).To(BeEmpty(), "a change is reported once")

	// The watched EnvoyFilter carrier needs no probe.
	probe.observe(defaultGateway, &tenantreconcile.RunResult{})
	g.Expect(c.Delete(t.Context(), plugin(defaultGateway))).To(Succeed())
	g.Expect(probe.changed(t.Context())).To(BeEmpty())
}
