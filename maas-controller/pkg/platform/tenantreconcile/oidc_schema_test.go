package tenantreconcile_test

import (
	"os"
	"path/filepath"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Tenant external OIDC admission schema", Ordered, func() {
	BeforeAll(func() {
		crdPath := filepath.Join(
			pkgtest.ProjectRoot(),
			"deployment", "base", "maas-controller", "crd", "bases", "maas.opendatahub.io_tenants.yaml",
		)
		data, err := os.ReadFile(crdPath)
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.UnmarshalStrict(data, crd)).To(Succeed())
		_, err = envtest.InstallCRDs(envTest.Config, envtest.CRDInstallOptions{
			CRDs: []*apiextensionsv1.CustomResourceDefinition{crd},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("requires the access token purpose fields to be set together", func(ctx SpecContext) {
		namespace := pkgtest.NewTestNamespace(ctx, envTest).Name

		for _, testCase := range []struct {
			name          string
			purposeClaim  string
			purpose       string
			expectInvalid bool
		}{
			{name: "both unset", purposeClaim: "", purpose: "", expectInvalid: false},
			{name: "claim only", purposeClaim: "token_use", purpose: "", expectInvalid: true},
			{name: "purpose only", purposeClaim: "", purpose: "access_token", expectInvalid: true},
			{name: "both set", purposeClaim: "token_use", purpose: "access_token", expectInvalid: false},
		} {
			tenant := &maasv1alpha1.Tenant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      maasv1alpha1.TenantInstanceName,
					Namespace: namespace,
				},
				Spec: maasv1alpha1.TenantSpec{
					ExternalOIDC: &maasv1alpha1.TenantExternalOIDCConfig{
						IssuerURL:               "https://issuer.example.com/realms/test",
						ClientID:                "maas-api",
						AccessTokenPurposeClaim: testCase.purposeClaim,
						AccessTokenPurpose:      testCase.purpose,
					},
				},
			}
			err := envTest.Create(ctx, tenant)
			if testCase.expectInvalid {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an admission rejection, got %v", err)
			} else {
				Expect(err).NotTo(HaveOccurred())
				Expect(envTest.Delete(ctx, tenant)).To(Succeed())
			}
		}
	})
})
