package tenantreconcile

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func TestBuildParamsMap_ReadsTemplateWithoutMutation(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	original := "maas-api-image=quay.io/opendatahub/maas-api:latest\ngateway-namespace=openshift-ingress\n"
	g.Expect(os.WriteFile(paramsFile, []byte(original), 0600)).To(Succeed())

	result, err := BuildParamsMap(dir, "params.env", map[string]string{}, map[string]string{
		"gateway-namespace": "custom-ns",
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result["gateway-namespace"]).To(Equal("custom-ns"))
	g.Expect(result["maas-api-image"]).To(Equal("quay.io/opendatahub/maas-api:latest"))

	after, err := os.ReadFile(paramsFile)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(after)).To(Equal(original), "on-disk params.env must not be mutated")
}

func TestBuildParamsMap_AppliesImageEnvVarOverrides(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	g.Expect(os.WriteFile(paramsFile, []byte("maas-api-image=default:latest\n"), 0600)).To(Succeed())

	t.Setenv("RELATED_IMAGE_ODH_MAAS_API_IMAGE", "custom-registry/maas-api:v2")

	result, err := BuildParamsMap(dir, "params.env",
		map[string]string{"maas-api-image": "RELATED_IMAGE_ODH_MAAS_API_IMAGE"},
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result["maas-api-image"]).To(Equal("custom-registry/maas-api:v2"))
}

func TestBuildParamsMap_MissingFileReturnsEmptyMap(t *testing.T) {
	g := NewWithT(t)

	result, err := BuildParamsMap(t.TempDir(), "params.env", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(BeEmpty())
}

func TestBuildParamsMap_ExtraParamsOverrideDefaults(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	g.Expect(os.WriteFile(paramsFile, []byte("app-namespace=opendatahub\ngateway-name=default-gw\n"), 0600)).To(Succeed())

	result, err := BuildParamsMap(dir, "params.env", nil,
		map[string]string{"app-namespace": "custom-ns"},
		map[string]string{"gateway-name": "custom-gw"},
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result["app-namespace"]).To(Equal("custom-ns"))
	g.Expect(result["gateway-name"]).To(Equal("custom-gw"))
}

func TestBuildParamsMap_SkipsCommentLines(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	content := "# This is a comment\nmaas-api-image=test:latest\n# Another comment\ngateway-name=gw\n"
	g.Expect(os.WriteFile(paramsFile, []byte(content), 0600)).To(Succeed())

	result, err := BuildParamsMap(dir, "params.env", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveLen(2))
	g.Expect(result["maas-api-image"]).To(Equal("test:latest"))
	g.Expect(result["gateway-name"]).To(Equal("gw"))
}
