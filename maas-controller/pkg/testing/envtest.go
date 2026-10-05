// Package testing runs controller-runtime's envtest, a bare API server and etcd, for
// specs that depend on real API server behaviour such as defaulting, update strategies
// and server-side apply. No controllers run, so nothing reconciles behind a spec.
package testing

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// AddToSchemeFunc registers types with a scheme.
type AddToSchemeFunc func(scheme *k8sruntime.Scheme) error

// Option configures the environment before it starts.
type Option func(target *envtest.Environment)

// Config collects the environment options until Start.
type Config struct {
	envTestOptions []Option
}

// Client is what specs talk to: a client for the envtest API server and the
// environment behind it.
type Client struct {
	client.Client
	*envtest.Environment
}

// Configure creates a configuration for the test environment.
func Configure(options ...Option) *Config {
	return &Config{envTestOptions: options}
}

// WithCRDs installs the CRDs found under paths before the specs run.
func WithCRDs(paths ...string) Option {
	return func(target *envtest.Environment) {
		target.CRDInstallOptions.Paths = append(target.CRDInstallOptions.Paths, paths...)
	}
}

// WithScheme replaces the environment scheme with one built from addToScheme.
func WithScheme(addToScheme ...AddToSchemeFunc) Option {
	return func(target *envtest.Environment) {
		scheme := k8sruntime.NewScheme()
		for _, add := range addToScheme {
			utilruntime.Must(add(scheme))
		}
		target.Scheme = scheme
		target.CRDInstallOptions.Scheme = scheme
	}
}

// Start runs the environment until the suite ends. Specs are skipped when
// KUBEBUILDER_ASSETS is unset; `make test` sets it through setup-envtest.
func (c *Config) Start() *Client {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		ginkgo.Skip("KUBEBUILDER_ASSETS is not set; run through make test")
	}

	env := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{ErrorIfPathMissing: true},
	}
	for _, opt := range c.envTestOptions {
		opt(env)
	}

	cfg, err := env.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(env.Stop)

	cli, err := client.New(cfg, client.Options{Scheme: env.Scheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	return &Client{Client: cli, Environment: env}
}

// Serves reports whether the API server serves gvk, so specs can leave out objects
// whose CRDs the environment does not install.
func (c *Client) Serves(gvk schema.GroupVersionKind) bool {
	_, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		return false
	}
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return true
}

// ProjectRoot returns the repository root, for specs that read manifests from the tree.
func ProjectRoot() string {
	_, file, _, ok := runtime.Caller(0)
	gomega.Expect(ok).To(gomega.BeTrue())
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}
