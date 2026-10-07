// Package docsamples_test admission-checks the samples published under docs/samples
// against a real API server.
//
// The samples are documentation: users run `kustomize build | kubectl apply` on them
// or apply single files. The e2e suite deploys only some of them, the GPU and large
// CPU ones never run in CI, so without this suite a renamed or retired field breaks
// them silently. Admission is the part that can be checked everywhere: CRD structural
// schema and the CEL rules compiled into the CRDs.
//
// What this suite deliberately does not cover: validating webhooks (KServe's do not
// run here), whether a sample reconciles, pulls its images or serves a token. Objects
// are submitted with DryRun, so nothing is persisted or reconciled.
package docsamples_test

import (
	"context"
	"path/filepath"
	"testing"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDocumentationSamples(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Documentation Samples Suite")
}

var envTest *pkgtest.Client

var _ = BeforeSuite(func(ctx SpecContext) {
	envTest = pkgtest.Configure(
		pkgtest.WithScheme(clientgoscheme.AddToScheme),
		pkgtest.WithCRDs(sampleCRDs(ctx)...),
	).Start()
})

// sampleCRDs lists the CRDs of every API group the samples use. Dependency CRDs come
// from the module versions go.mod resolves, so they move with dependency bumps.
func sampleCRDs(ctx context.Context) []string {
	kserve := pkgtest.ModuleDir(ctx, "github.com/kserve/kserve")
	gatewayAPI := pkgtest.ModuleDir(ctx, "sigs.k8s.io/gateway-api")
	openshiftAPI := pkgtest.ModuleDir(ctx, "github.com/openshift/api")

	return []string{
		filepath.Join(pkgtest.ProjectRoot(), "deployment", "base", "maas-controller", "crd", "bases"),
		filepath.Join(kserve, "config", "crd", "full", "llmisvc", "serving.kserve.io_llminferenceservices.yaml"),
		filepath.Join(gatewayAPI, "config", "crd", "standard"),
		filepath.Join(openshiftAPI, "route", "v1", "zz_generated.crd-manifests", "routes.crd.yaml"),
	}
}
