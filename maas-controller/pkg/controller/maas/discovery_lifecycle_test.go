//nolint:testpackage
package maas

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func discoveryManifestPath(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve discovery test file path")
	}
	return filepath.Join(filepath.Dir(testFile), "../../../../deployment/base/maas-discovery")
}

func TestEnsureDiscoveryService(t *testing.T) {
	const (
		controllerNS = "opendatahub"
		discoveryNS  = "odh-ai-gateway-infra"
		aitenantNS   = "ai-tenants"
		gatewayNS    = "openshift-ingress"
		gatewayName  = "data-science-gateway"
		testImage    = "quay.io/test/odh-maas-discovery:v1"
	)

	gvkDeployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	gvkService := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}
	gvkRole := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}
	gvkRoleBinding := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}

	manifestPath := discoveryManifestPath(t)

	t.Run("skips when manifest path is empty", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cl := fake.NewClientBuilder().WithScheme(s).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DiscoveryManifestPath: "",
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("skips when Config does not exist", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("enabled applies resources with correct namespace", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDeployment)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)).
			To(Succeed(), "Deployment should be created in discovery namespace")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkService)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)).
			To(Succeed(), "Service should be created in discovery namespace")
	})

	t.Run("enabled patches image correctly", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDeployment)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)).To(Succeed())

		containers, found, err := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(containers).NotTo(BeEmpty())
		cm, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(cm["image"]).To(Equal(testImage))
	})

	t.Run("enabled patches replicas when configured", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		replicas := int32(5)
		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
			DiscoveryReplicas:     &replicas,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDeployment)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)).To(Succeed())

		gotReplicas, found, err := unstructured.NestedInt64(got.Object, "spec", "replicas")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(gotReplicas).To(Equal(int64(5)))
	})

	t.Run("enabled creates cross-namespace RBAC", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkRole)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: aitenantNS}, got)).
			To(Succeed(), "Role should be created in ai-tenants namespace")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkRoleBinding)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: aitenantNS}, got)).
			To(Succeed(), "RoleBinding should be created in ai-tenants namespace")

		subjects, found, err := unstructured.NestedSlice(got.Object, "subjects")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(subjects).NotTo(BeEmpty())
		subj, ok := subjects[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(subj["namespace"]).To(Equal(discoveryNS))

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkRole)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: gatewayNS}, got)).
			To(Succeed(), "Role should be created in gateway namespace")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkRoleBinding)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: gatewayNS}, got)).
			To(Succeed(), "RoleBinding should be created in gateway namespace")
	})

	t.Run("disabled deletes owned resources", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		ownedDep := &unstructured.Unstructured{}
		ownedDep.SetGroupVersionKind(gvkDeployment)
		ownedDep.SetName(discoveryDeploymentName)
		ownedDep.SetNamespace(discoveryNS)
		ownedDep.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        cfg.UID,
			Controller: ptr.To(true),
		}})

		ownedRole := &unstructured.Unstructured{}
		ownedRole.SetGroupVersionKind(gvkRole)
		ownedRole.SetName(discoveryDeploymentName)
		ownedRole.SetNamespace(aitenantNS)
		ownedRole.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        cfg.UID,
			Controller: ptr.To(true),
		}})

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg, ownedDep, ownedRole).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      false,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDeployment)
		err = cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned Deployment should be deleted")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkRole)
		err = cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: aitenantNS}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned Role should be deleted")
	})

	t.Run("disabled preserves unowned resources", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		foreignDep := &unstructured.Unstructured{}
		foreignDep.SetGroupVersionKind(gvkDeployment)
		foreignDep.SetName(discoveryDeploymentName)
		foreignDep.SetNamespace(discoveryNS)

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg, foreignDep).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gatewayName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      false,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDeployment)
		err = cl.Get(context.Background(), client.ObjectKey{Name: discoveryDeploymentName, Namespace: discoveryNS}, got)
		g.Expect(err).NotTo(HaveOccurred(), "foreign Deployment should be preserved (CWE-284)")
	})
}

func TestPatchDiscoveryImage(t *testing.T) {
	t.Run("patches maas-discovery container image", func(t *testing.T) {
		g := NewWithT(t)

		dep := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": discoveryDeploymentName},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": discoveryContainerName, "image": "placeholder"},
						},
					},
				},
			},
		}}

		err := patchDiscoveryImage(dep, "quay.io/test/discovery:v2")
		g.Expect(err).NotTo(HaveOccurred())

		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		cm, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(cm["image"]).To(Equal("quay.io/test/discovery:v2"))
	})

	t.Run("uses default when image is empty", func(t *testing.T) {
		g := NewWithT(t)

		dep := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": discoveryDeploymentName},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": discoveryContainerName, "image": "placeholder"},
						},
					},
				},
			},
		}}

		err := patchDiscoveryImage(dep, "")
		g.Expect(err).NotTo(HaveOccurred())

		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		cm, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(cm["image"]).To(Equal(DefaultMaaSDiscoveryImage))
	})

	t.Run("skips non-deployment resources", func(t *testing.T) {
		g := NewWithT(t)

		svc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]any{"name": discoveryDeploymentName},
		}}

		err := patchDiscoveryImage(svc, "quay.io/test/discovery:v2")
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func TestPatchDiscoveryReplicas(t *testing.T) {
	t.Run("patches replicas when set", func(t *testing.T) {
		g := NewWithT(t)

		dep := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": discoveryDeploymentName},
			"spec":       map[string]any{"replicas": int64(2)},
		}}

		r := int32(3)
		patchDiscoveryReplicas(dep, &r)

		gotReplicas, found, err := unstructured.NestedInt64(dep.Object, "spec", "replicas")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(gotReplicas).To(Equal(int64(3)))
	})

	t.Run("preserves default when nil", func(t *testing.T) {
		g := NewWithT(t)

		dep := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": discoveryDeploymentName},
			"spec":       map[string]any{"replicas": int64(2)},
		}}

		patchDiscoveryReplicas(dep, nil)

		gotReplicas, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas")
		g.Expect(gotReplicas).To(Equal(int64(2)))
	})
}

func TestBuildDiscoveryCrossNamespaceRBAC(t *testing.T) {
	g := NewWithT(t)

	resources := buildDiscoveryCrossNamespaceRBAC("opendatahub", "ai-tenants", "openshift-ingress")
	g.Expect(resources).To(HaveLen(4))

	g.Expect(resources[0].GetKind()).To(Equal("Role"))
	g.Expect(resources[0].GetNamespace()).To(Equal("ai-tenants"))

	g.Expect(resources[1].GetKind()).To(Equal("RoleBinding"))
	g.Expect(resources[1].GetNamespace()).To(Equal("ai-tenants"))

	g.Expect(resources[2].GetKind()).To(Equal("Role"))
	g.Expect(resources[2].GetNamespace()).To(Equal("openshift-ingress"))

	g.Expect(resources[3].GetKind()).To(Equal("RoleBinding"))
	g.Expect(resources[3].GetNamespace()).To(Equal("openshift-ingress"))

	subjects, found, err := unstructured.NestedSlice(resources[1].Object, "subjects")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	subj, ok := subjects[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(subj["namespace"]).To(Equal("opendatahub"))
	g.Expect(subj["name"]).To(Equal(discoveryDeploymentName))
}

func TestBuildDiscoveryGatewayResources(t *testing.T) {
	g := NewWithT(t)

	resources := buildDiscoveryGatewayResources("odh-ai-gateway-infra", "openshift-ingress", "https://test-audience.example.com")
	g.Expect(resources).To(HaveLen(2))

	dr := resources[0]
	g.Expect(dr.GetKind()).To(Equal("DestinationRule"))
	g.Expect(dr.GetName()).To(Equal(discoveryDestinationRuleName))
	g.Expect(dr.GetNamespace()).To(Equal("openshift-ingress"))

	host, _, _ := unstructured.NestedString(dr.Object, "spec", "host")
	g.Expect(host).To(Equal("maas-discovery.odh-ai-gateway-infra.svc.cluster.local"))

	tlsMode, _, _ := unstructured.NestedString(dr.Object, "spec", "trafficPolicy", "portLevelSettings", "0", "tls", "mode")
	g.Expect(tlsMode).To(BeEmpty(), "portLevelSettings is a slice, access via NestedSlice instead")

	pls, found, _ := unstructured.NestedSlice(dr.Object, "spec", "trafficPolicy", "portLevelSettings")
	g.Expect(found).To(BeTrue())
	g.Expect(pls).To(HaveLen(1))
	entry, ok := pls[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	tlsMap, ok := entry["tls"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(tlsMap["mode"]).To(Equal("SIMPLE"))
	g.Expect(tlsMap["insecureSkipVerify"]).To(BeTrue())

	ap := resources[1]
	g.Expect(ap.GetKind()).To(Equal("AuthPolicy"))
	g.Expect(ap.GetName()).To(Equal(discoveryAuthPolicyName))
	g.Expect(ap.GetNamespace()).To(Equal("odh-ai-gateway-infra"))

	targetName, _, _ := unstructured.NestedString(ap.Object, "spec", "targetRef", "name")
	g.Expect(targetName).To(Equal(discoveryHTTPRouteName))

	targetKind, _, _ := unstructured.NestedString(ap.Object, "spec", "targetRef", "kind")
	g.Expect(targetKind).To(Equal("HTTPRoute"))

	audiences, found, _ := unstructured.NestedStringSlice(ap.Object, "spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences")
	g.Expect(found).To(BeTrue())
	g.Expect(audiences).To(ConsistOf("https://test-audience.example.com"))
}

func TestPatchDiscoveryHTTPRouteParentRef(t *testing.T) {
	t.Run("patches parentRef gateway name and namespace", func(t *testing.T) {
		g := NewWithT(t)

		route := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata":   map[string]any{"name": discoveryHTTPRouteName},
			"spec": map[string]any{
				"parentRefs": []any{
					map[string]any{
						"name":      "data-science-gateway",
						"namespace": "openshift-ingress",
					},
				},
			},
		}}

		patchDiscoveryHTTPRouteParentRef(route, "my-gateway", "my-ns")

		parentRefs, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
		g.Expect(parentRefs).To(HaveLen(1))
		ref, ok := parentRefs[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(ref["name"]).To(Equal("my-gateway"))
		g.Expect(ref["namespace"]).To(Equal("my-ns"))
	})

	t.Run("skips non-HTTPRoute resources", func(t *testing.T) {
		g := NewWithT(t)

		svc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]any{"name": discoveryHTTPRouteName},
		}}

		patchDiscoveryHTTPRouteParentRef(svc, "my-gateway", "my-ns")
		g.Expect(svc.GetKind()).To(Equal("Service"))
	})

	t.Run("skips other HTTPRoutes", func(t *testing.T) {
		g := NewWithT(t)

		route := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata":   map[string]any{"name": "some-other-route"},
			"spec": map[string]any{
				"parentRefs": []any{
					map[string]any{
						"name":      "original-gw",
						"namespace": "original-ns",
					},
				},
			},
		}}

		patchDiscoveryHTTPRouteParentRef(route, "my-gateway", "my-ns")

		parentRefs, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
		ref, ok := parentRefs[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(ref["name"]).To(Equal("original-gw"))
	})
}

func TestEnsureDiscoveryServiceGatewayResources(t *testing.T) {
	const (
		controllerNS = "opendatahub"
		discoveryNS  = "odh-ai-gateway-infra"
		aitenantNS   = "ai-tenants"
		gatewayNS    = "openshift-ingress"
		gwName       = "data-science-gateway"
		testImage    = "quay.io/test/maas-discovery:v1"
	)

	gvkDestinationRule := schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"}
	gvkAuthPolicy := schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}
	gvkHTTPRoute := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

	manifestPath := discoveryManifestPath(t)

	t.Run("enabled creates DestinationRule in gateway namespace", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gwName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDestinationRule)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryDestinationRuleName, Namespace: gatewayNS}, got)).
			To(Succeed(), "DestinationRule should be created in gateway namespace")

		host, _, _ := unstructured.NestedString(got.Object, "spec", "host")
		g.Expect(host).To(Equal("maas-discovery." + discoveryNS + ".svc.cluster.local"))
	})

	t.Run("enabled creates AuthPolicy in discovery namespace", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gwName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkAuthPolicy)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryAuthPolicyName, Namespace: discoveryNS}, got)).
			To(Succeed(), "AuthPolicy should be created in discovery namespace")

		targetName, _, _ := unstructured.NestedString(got.Object, "spec", "targetRef", "name")
		g.Expect(targetName).To(Equal(discoveryHTTPRouteName))
	})

	t.Run("enabled patches HTTPRoute parentRef", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gwName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      true,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkHTTPRoute)
		g.Expect(cl.Get(context.Background(), client.ObjectKey{Name: discoveryHTTPRouteName, Namespace: discoveryNS}, got)).
			To(Succeed(), "HTTPRoute should be created")

		parentRefs, found, _ := unstructured.NestedSlice(got.Object, "spec", "parentRefs")
		g.Expect(found).To(BeTrue())
		ref, ok := parentRefs[0].(map[string]any)
		g.Expect(ok).To(BeTrue())
		g.Expect(ref["name"]).To(Equal(gwName))
		g.Expect(ref["namespace"]).To(Equal(gatewayNS))
	})

	t.Run("disabled deletes gateway resources", func(t *testing.T) {
		g := NewWithT(t)
		s := lifecycleTestScheme(t)

		cfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		}

		ownedDR := &unstructured.Unstructured{}
		ownedDR.SetGroupVersionKind(gvkDestinationRule)
		ownedDR.SetName(discoveryDestinationRuleName)
		ownedDR.SetNamespace(gatewayNS)
		ownedDR.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        cfg.UID,
			Controller: ptr.To(true),
		}})

		ownedAP := &unstructured.Unstructured{}
		ownedAP.SetGroupVersionKind(gvkAuthPolicy)
		ownedAP.SetName(discoveryAuthPolicyName)
		ownedAP.SetNamespace(discoveryNS)
		ownedAP.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        cfg.UID,
			Controller: ptr.To(true),
		}})

		cl := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&maasv1alpha1.Config{}).WithObjects(cfg, ownedDR, ownedAP).Build()
		r := &LifecycleReconciler{
			Client:                cl,
			Scheme:                s,
			DeploymentNS:          controllerNS,
			AITenantNamespace:     aitenantNS,
			DiscoveryGatewayName:  gwName,
			GatewayNamespace:      gatewayNS,
			DiscoveryEnabled:      false,
			DiscoveryManifestPath: manifestPath,
			DiscoveryImage:        testImage,
			DiscoveryNamespace:    discoveryNS,
		}

		err := r.ensureDiscoveryService(context.Background(), ctrl.Log)
		g.Expect(err).NotTo(HaveOccurred())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkDestinationRule)
		err = cl.Get(context.Background(), client.ObjectKey{Name: discoveryDestinationRuleName, Namespace: gatewayNS}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned DestinationRule should be deleted")

		got = &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvkAuthPolicy)
		err = cl.Get(context.Background(), client.ObjectKey{Name: discoveryAuthPolicyName, Namespace: discoveryNS}, got)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned AuthPolicy should be deleted")
	})
}
