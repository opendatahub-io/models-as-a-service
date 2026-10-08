package tenantreconcile_test

import (
	"fmt"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestInstalledInferenceCRDsMatchAGC(t *testing.T) {
	g := NewWithT(t)
	crds, err := fixture.InferenceCRDs()
	g.Expect(err).NotTo(HaveOccurred())
	expected, err := fixture.InferenceSchemaContract()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(expected).To(HaveLen(len(crds)))
	for _, crd := range crds {
		t.Run(crd.Spec.Names.Kind, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(crd.Spec.Versions).To(HaveLen(1))
			g.Expect(crd.Spec.Versions[0].Name).To(Equal("v1alpha1"))
			actual := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.DeepCopy()
			stripSchemaDescriptions(actual)
			g.Expect(*actual).To(Equal(expected[crd.Spec.Names.Plural]), "installed inference schema drifted from AGC")
		})
	}
}

func stripSchemaDescriptions(schema *apiextensionsv1.JSONSchemaProps) {
	schema.Description = ""
	for name, property := range schema.Properties {
		stripSchemaDescriptions(&property)
		schema.Properties[name] = property
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		stripSchemaDescriptions(schema.Items.Schema)
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Schema != nil {
		stripSchemaDescriptions(schema.AdditionalProperties.Schema)
	}
}

var _ = Describe("Installed inference API schemas", Ordered, func() {
	var namespace string

	BeforeAll(func(ctx SpecContext) {
		namespace = pkgtest.NewTestNamespace(ctx, envTest).Name
		legacy, err := fixture.LegacyInferenceCRDs()
		Expect(err).NotTo(HaveOccurred())
		_, err = envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{CRDs: legacy})
		Expect(err).NotTo(HaveOccurred())
	})

	It("preserves legacy objects and status when upgrading the installed schemas", func(ctx SpecContext) {
		provider := fixture.InferenceProvider(namespace)
		model := fixture.InferenceModel(namespace, "legacy")
		Expect(envTest.Create(ctx, provider)).To(Succeed())
		Expect(envTest.Create(ctx, model)).To(Succeed())
		model.Object["status"] = map[string]any{"phase": "Ready", "httpRouteName": "legacy-route"}
		Expect(envTest.Status().Update(ctx, model)).To(Succeed())
		beforeProvider, beforeModel := provider.DeepCopy(), model.DeepCopy()

		updated, err := fixture.InferenceCRDs()
		Expect(err).NotTo(HaveOccurred())
		_, err = envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{CRDs: updated})
		Expect(err).NotTo(HaveOccurred())
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(model), model)).To(Succeed())
		Expect(provider.Object["spec"]).To(Equal(beforeProvider.Object["spec"]))
		Expect(model.Object["spec"]).To(Equal(beforeModel.Object["spec"]))
		Expect(model.Object["status"]).To(Equal(beforeModel.Object["status"]))
		for _, obj := range []*unstructured.Unstructured{provider, model} {
			obj.SetAnnotations(map[string]string{"updated": "true"})
			Expect(envTest.Update(ctx, obj)).To(Succeed())
		}
		Expect(model.Object["spec"]).NotTo(HaveKey("gatewayRefs"))
		provider.Object["status"] = map[string]any{"observedGeneration": provider.GetGeneration()}
		Expect(envTest.Status().Update(ctx, provider)).To(Succeed())
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
		Expect(provider.Object["status"]).To(HaveKeyWithValue("observedGeneration", provider.GetGeneration()))
	})

	It("stores gateway identities, provider namespaces and complete route status", func(ctx SpecContext) {
		model := fixture.InferenceModel(namespace, "attached")
		gateways := []any{map[string]any{"name": "shared", "namespace": "tenant-a"}, map[string]any{"name": "shared", "namespace": "tenant-b"}}
		Expect(unstructured.SetNestedSlice(model.Object, gateways, "spec", "gatewayRefs")).To(Succeed())
		refs, _, err := unstructured.NestedSlice(model.Object, "spec", "externalProviderRefs")
		Expect(err).NotTo(HaveOccurred())
		providerRef, ok := refs[0].(map[string]any)
		Expect(ok).To(BeTrue())
		expectedProviderRef := map[string]any{"name": "provider", "namespace": "shared-providers"}
		providerRef["ref"] = expectedProviderRef
		Expect(unstructured.SetNestedSlice(model.Object, refs, "spec", "externalProviderRefs")).To(Succeed())
		Expect(envTest.Create(ctx, model)).To(Succeed())
		condition := map[string]any{
			"type": "Ready", "status": "True", "reason": "Reconciled", "message": "Distributed",
			"observedGeneration": model.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		}
		status := map[string]any{
			"observedGeneration": model.GetGeneration(), "overlayDigest": "sha256:test", "overlayGeneration": int64(7),
			"gateways": []any{map[string]any{
				"name": "shared", "namespace": "tenant-a",
				"httpRouteRef": map[string]any{"name": "route", "namespace": "routes"},
				"conditions":   []any{condition},
			}},
		}
		model.Object["status"] = status
		Expect(envTest.Status().Update(ctx, model)).To(Succeed())
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(model), model)).To(Succeed())
		actualGateways, found, err := unstructured.NestedSlice(model.Object, "spec", "gatewayRefs")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(actualGateways).To(Equal(gateways))
		actualRefs, _, err := unstructured.NestedSlice(model.Object, "spec", "externalProviderRefs")
		Expect(err).NotTo(HaveOccurred())
		Expect(actualRefs[0]).To(HaveKeyWithValue("ref", expectedProviderRef))
		Expect(model.Object["status"]).To(Equal(status))
		Expect(unstructured.SetNestedField(model.Object, "changed", "spec", "modelName")).To(Succeed())
		Expect(envTest.Update(ctx, model)).To(Succeed())
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(model), model)).To(Succeed())
		Expect(model.Object["status"]).To(Equal(status))
	})

	It("rejects invalid attachment lists through the installed admission schema", func(ctx SpecContext) {
		many := make([]any, 17)
		for i := range many {
			many[i] = map[string]any{"name": fmt.Sprintf("gateway-%d", i), "namespace": "tenant"}
		}
		duplicate := map[string]any{"name": "same", "namespace": "tenant"}
		for i, refs := range [][]any{{}, {duplicate, duplicate}, many} {
			model := fixture.InferenceModel(namespace, fmt.Sprintf("invalid-%d", i))
			Expect(unstructured.SetNestedSlice(model.Object, refs, "spec", "gatewayRefs")).To(Succeed())
			err := envTest.Create(ctx, model)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an admission rejection, got %v", err)
		}
	})
})
