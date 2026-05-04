package tenantreconcile

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func TestWriteParamsFile_SortedDeterministicOutput(t *testing.T) {
	g := NewWithT(t)

	path := filepath.Join(t.TempDir(), "params.env")
	params := map[string]string{
		"z-key": "z-val",
		"a-key": "a-val",
		"m-key": "m-val",
	}

	g.Expect(writeParamsFile(path, params)).To(Succeed())

	content, err := os.ReadFile(path)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(content)).To(Equal("a-key=a-val\nm-key=m-val\nz-key=z-val\n"))
}

func TestRenderKustomizeWithParams_RestoresOriginal(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")
	original := "gateway-namespace=openshift-ingress\napp-namespace=opendatahub\n"
	g.Expect(os.WriteFile(paramsFile, []byte(original), 0600)).To(Succeed())

	// RenderKustomizeWithParams will fail because there's no kustomization.yaml,
	// but it should still restore the original params.env.
	_, err := RenderKustomizeWithParams(dir, "test-ns", map[string]string{
		"gateway-namespace": "custom",
		"app-namespace":     "custom-ns",
	})
	g.Expect(err).To(HaveOccurred(), "expected kustomize build to fail (no kustomization.yaml)")

	after, err := os.ReadFile(paramsFile)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(after)).To(Equal(original), "params.env must be restored after kustomize build")
}

func TestRenderKustomizeWithParams_CleansUpWhenNoOriginal(t *testing.T) {
	g := NewWithT(t)

	dir := t.TempDir()
	paramsFile := filepath.Join(dir, "params.env")

	_, err := RenderKustomizeWithParams(dir, "test-ns", map[string]string{
		"key": "value",
	})
	g.Expect(err).To(HaveOccurred(), "expected kustomize build to fail (no kustomization.yaml)")

	_, statErr := os.Stat(paramsFile)
	g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "params.env should be removed if it didn't exist before")
}
