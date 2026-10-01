package fixture

import (
	"context"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"
)

// TenantConfig returns the MaasTenantConfig the AITenant named name manages.
func TenantConfig(name string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: tenantreconcile.TenantNamespaceForAITenant(name, DefaultTenantConfig().Namespace),
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        name,
			},
		},
	}
}

// RenderTenant renders the platform overlay for tenant with the PlatformParams its
// reconcile builds, so renamed objects carry that tenant's tracking labels and the rest
// the shared-operand marker. Every tenant renders with the same gateway, images and
// namespaces unless opts say otherwise.
func RenderTenant(ctx context.Context, tenant *maasv1alpha1.MaasTenantConfig, opts ...RenderOption) []unstructured.Unstructured {
	platformContext := tenantreconcile.PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: "openshift-ingress", Name: "maas-default-gateway"},
	}
	params, err := tenantreconcile.BuildPlatformParams(tenant, platformContext,
		"maas-infra", "controller-ns", "https://kubernetes.default.svc", "opendatahub", logr.Discard())
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	cfg := &renderConfig{overlay: "odh", params: params}
	for _, opt := range opts {
		opt(cfg)
	}

	overlayDir := filepath.Join(pkgtest.ProjectRoot(), "maas-api", "deploy", "overlays", cfg.overlay)
	rendered, err := tenantreconcile.RenderKustomize(overlayDir, cfg.params.AppNamespace)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	operands, err := tenantreconcile.PostRender(ctx, logr.Discard(), tenant, rendered, cfg.params)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(operands).NotTo(gomega.BeEmpty())
	return operands
}

// ApplyTrackingLabelled applies objs the way ApplyRendered did before the shared-operand
// marker: every object carries tenant's tracking labels and none carries the marker. It
// stands up the state a controller upgrade starts from.
func ApplyTrackingLabelled(ctx context.Context, c client.Client, scheme *runtime.Scheme, tenant client.Object, appNamespace string, objs []unstructured.Unstructured) {
	labelled := make([]unstructured.Unstructured, len(objs))
	for i := range objs {
		obj := objs[i].DeepCopy()
		labels := obj.GetLabels()
		delete(labels, tenantreconcile.LabelSharedOperand)
		obj.SetLabels(labels)
		tenantreconcile.SetTenantTrackingLabels(obj, tenant)
		labelled[i] = *obj
	}
	gomega.Expect(tenantreconcile.ApplyRendered(ctx, c, scheme, appNamespace, ConfigAnchor(), labelled)).To(gomega.Succeed())
}
