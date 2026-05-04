package tenantreconcile

import (
	"os"
	"path/filepath"
	"testing"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func TestBuildCustomizedParams_MergesTenantValues(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	g.Expect(os.WriteFile(paramsFile, []byte(
		"maas-api-image=quay.io/opendatahub/maas-api:latest\n"+
			"gateway-namespace=openshift-ingress\n"+
			"gateway-name=maas-default-gateway\n"+
			"app-namespace=opendatahub\n"+
			"cluster-audience=https://kubernetes.default.svc\n",
	), 0600)).To(Succeed())

	days := int32(30)
	tenant := &maasv1alpha1.Tenant{
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "custom-ingress",
				Name:      "my-gateway",
			},
			APIKeys: &maasv1alpha1.TenantAPIKeysConfig{
				MaxExpirationDays: &days,
			},
		},
	}

	result, err := BuildCustomizedParams(dir, tenant, "my-ns", "https://custom.issuer")
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(result["gateway-namespace"]).To(Equal("custom-ingress"))
	g.Expect(result["gateway-name"]).To(Equal("my-gateway"))
	g.Expect(result["app-namespace"]).To(Equal("my-ns"))
	g.Expect(result["cluster-audience"]).To(Equal("https://custom.issuer"))
	g.Expect(result["api-key-max-expiration-days"]).To(Equal("30"))
	g.Expect(result["maas-api-image"]).To(Equal("quay.io/opendatahub/maas-api:latest"))

	after, err := os.ReadFile(paramsFile)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(after)).To(ContainSubstring("gateway-namespace=openshift-ingress"),
		"on-disk params.env must not be mutated by BuildCustomizedParams")
}

func TestBuildCustomizedParams_NoAPIKeys(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	g.Expect(os.WriteFile(paramsFile, []byte(
		"gateway-namespace=openshift-ingress\ngateway-name=gw\napp-namespace=odh\n",
	), 0600)).To(Succeed())

	tenant := &maasv1alpha1.Tenant{
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Namespace: "ns",
				Name:      "gw",
			},
		},
	}

	result, err := BuildCustomizedParams(dir, tenant, "odh", "")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).NotTo(HaveKey("api-key-max-expiration-days"))
	g.Expect(result).NotTo(HaveKey("cluster-audience"))
}
