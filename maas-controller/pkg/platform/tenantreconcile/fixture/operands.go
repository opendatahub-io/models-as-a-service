package fixture

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"
)

// RenderOption customizes what RenderDefaultTenant renders.
type RenderOption func(*renderConfig)

type renderConfig struct {
	overlay string
	params  tenantreconcile.PlatformParams
}

// WithOverlay picks the platform overlay under maas-api/deploy/overlays.
func WithOverlay(name string) RenderOption {
	return func(cfg *renderConfig) {
		cfg.overlay = name
	}
}

// WithBundledPostgres renders for the in-cluster Postgres instead of an external database.
func WithBundledPostgres() RenderOption {
	return func(cfg *renderConfig) {
		cfg.params.BundledPostgres = true
	}
}

// InNamespaces places the operands in the given app and gateway namespaces.
func InNamespaces(app, gateway string) RenderOption {
	return func(cfg *renderConfig) {
		cfg.params.AppNamespace = app
		cfg.params.GatewayNamespace = gateway
	}
}

// DefaultTenantConfig returns the default tenant's MaasTenantConfig.
func DefaultTenantConfig() *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service"},
	}
}

// ConfigAnchor returns the Config that operands name as their controller owner.
// envtest runs no garbage collector, so it never has to exist.
func ConfigAnchor() *maasv1alpha1.Config {
	return &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: "envtest-config"},
	}
}

// RenderDefaultTenant renders the platform overlay and post-renders it the way the
// default tenant reconcile does, with placeholder images.
func RenderDefaultTenant(ctx context.Context, opts ...RenderOption) []unstructured.Unstructured {
	cfg := &renderConfig{
		overlay: "odh",
		params: tenantreconcile.PlatformParams{ //nolint:gosec // APIKeyMaxExpirationDays is a duration setting, not a secret
			AppNamespace:            "maas-infra",
			ControllerNamespace:     "controller-ns",
			GatewayNamespace:        "openshift-ingress",
			GatewayName:             "maas-default-gateway",
			MonitoringNamespace:     "opendatahub",
			SubscriptionNamespace:   "models-as-a-service",
			MaaSAPIImage:            "quay.io/example/maas-api:test",
			PayloadProcessingImage:  "quay.io/example/payload:test",
			MaaSAPIKeyCleanupImage:  "quay.io/example/cleanup:test",
			APIKeyMaxExpirationDays: "45",
		},
	}
	for _, opt := range opts {
		opt(cfg)
	}

	overlayDir := filepath.Join(pkgtest.ProjectRoot(), "maas-api", "deploy", "overlays", cfg.overlay)
	rendered, err := tenantreconcile.RenderKustomize(overlayDir, cfg.params.AppNamespace)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	operands, err := tenantreconcile.PostRender(ctx, logr.Discard(), DefaultTenantConfig(), rendered, cfg.params)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(operands).NotTo(gomega.BeEmpty())
	return operands
}

// EmptyListPaths lists the fields of obj that hold an empty list.
func EmptyListPaths(obj unstructured.Unstructured) []string {
	return emptyListPaths(obj.Object, "")
}

func emptyListPaths(v any, path string) []string {
	var paths []string
	switch v := v.(type) {
	case map[string]any:
		for key, child := range v {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			paths = append(paths, emptyListPaths(child, childPath)...)
		}
	case []any:
		if len(v) == 0 {
			return []string{path}
		}
		for i, child := range v {
			paths = append(paths, emptyListPaths(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return paths
}

// ResourceVersions reads the live resourceVersion of every object, keyed by kind,
// namespace and name.
func ResourceVersions(ctx context.Context, c client.Reader, objs []unstructured.Unstructured) map[string]string {
	versions := make(map[string]string, len(objs))
	for _, obj := range objs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(&obj), live)).To(gomega.Succeed())
		versions[fmt.Sprintf("%s %s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())] = live.GetResourceVersion()
	}
	return versions
}
