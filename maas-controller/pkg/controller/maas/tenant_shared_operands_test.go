package maas

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func TestIsSharedTenantOperand(t *testing.T) {
	tests := []struct {
		name   string
		gk     schema.GroupKind
		object string
		owner  string
		want   bool
	}{
		{name: "per-tenant Service of its owner", gk: tenantreconcile.GVKService.GroupKind(), object: "maas-api-red", owner: "red", want: false},
		{name: "default tenant's base-named Service", gk: tenantreconcile.GVKService.GroupKind(), object: "maas-api", owner: tenantreconcile.DefaultAITenantName, want: false},
		{name: "legacy default tenant config's base-named Service", gk: tenantreconcile.GVKService.GroupKind(), object: "maas-api", owner: maasv1alpha1.MaasTenantConfigInstanceName, want: false},
		{name: "Service rendered alike for every tenant", gk: tenantreconcile.GVKService.GroupKind(), object: "maas-api-metrics", owner: "red", want: true},
		{name: "per-tenant name under another tenant's labels", gk: tenantreconcile.GVKService.GroupKind(), object: "maas-api-blue", owner: "red", want: true},
		{name: "ServiceAccount rendered alike for every tenant", gk: tenantreconcile.GVKServiceAccount.GroupKind(), object: "maas-api", owner: "red", want: true},
		{name: "per-tenant ServiceAccount", gk: tenantreconcile.GVKServiceAccount.GroupKind(), object: "payload-processing-red", owner: "red", want: false},
		{name: "per-tenant NetworkPolicy", gk: tenantreconcile.GVKNetworkPolicy.GroupKind(), object: "payload-processing-red", owner: "red", want: false},
		{name: "NetworkPolicy rendered alike for every tenant", gk: tenantreconcile.GVKNetworkPolicy.GroupKind(), object: "maas-api-allow-gateway", owner: "red", want: true},
		{name: "ClusterRole, never renamed", gk: rbacv1.SchemeGroupVersion.WithKind("ClusterRole").GroupKind(), object: "payload-processing-reader", owner: "red", want: true},
		{name: "per-tenant ClusterRoleBinding", gk: tenantreconcile.GVKClusterRoleBinding.GroupKind(), object: "payload-processing-reader-red", owner: "red", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			obj := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Name:   tt.object,
				Labels: map[string]string{tenantreconcile.LabelTenantName: tt.owner},
			}}
			g.Expect(isSharedTenantOperand(tt.gk, obj)).To(Equal(tt.want))
		})
	}
}

// A rendered operand is tenant-specific exactly when another tenant's render names it
// differently, so a renamed operand missing from tenantRenderedNames fails here.
func TestTenantRenderedNamesMatchRenderedOperands(t *testing.T) {
	g := NewWithT(t)
	defaultOperands := sharedOperandRender(t, "")
	redOperands := sharedOperandRender(t, "red")

	namesOf := func(operands []unstructured.Unstructured) map[schema.GroupKind]map[string]bool {
		names := map[schema.GroupKind]map[string]bool{}
		for _, obj := range operands {
			gk := obj.GroupVersionKind().GroupKind()
			if names[gk] == nil {
				names[gk] = map[string]bool{}
			}
			names[gk][obj.GetName()] = true
		}
		return names
	}
	defaultNames, redNames := namesOf(defaultOperands), namesOf(redOperands)

	check := func(operands []unstructured.Unstructured, owner string, otherNames map[schema.GroupKind]map[string]bool) {
		for i := range operands {
			obj := &operands[i]
			gk := obj.GroupVersionKind().GroupKind()
			obj.SetLabels(map[string]string{tenantreconcile.LabelTenantName: owner})
			g.Expect(isSharedTenantOperand(gk, obj)).To(Equal(otherNames[gk][obj.GetName()]),
				"%s %s owned by %q", gk.Kind, obj.GetName(), owner)
		}
	}
	check(redOperands, "red", defaultNames)
	check(defaultOperands, tenantreconcile.DefaultAITenantName, redNames)
}

func sharedOperandRender(t *testing.T, tenantID string) []unstructured.Unstructured {
	t.Helper()
	g := NewWithT(t)

	_, file, _, ok := runtime.Caller(0)
	g.Expect(ok).To(BeTrue())
	overlay := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "maas-api", "deploy", "overlays", "odh")
	rendered, err := tenantreconcile.RenderKustomize(overlay, "maas-infra")
	g.Expect(err).NotTo(HaveOccurred())

	tenant := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
		Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service",
	}}
	if tenantID != "" {
		tenant.Labels = map[string]string{
			tenantreconcile.LabelManagedByAITenant: "true",
			tenantreconcile.LabelTenantName:        tenantID,
		}
	}
	params := tenantreconcile.PlatformParams{ //nolint:gosec // APIKeyMaxExpirationDays is a duration setting, not a secret
		AppNamespace:                 "maas-infra",
		ControllerNamespace:          "controller-ns",
		GatewayNamespace:             "openshift-ingress",
		GatewayName:                  "maas-default-gateway",
		MonitoringNamespace:          "opendatahub",
		SubscriptionNamespace:        "models-as-a-service",
		TenantIdentifier:             tenantID,
		MaaSAPIImage:                 "quay.io/example/maas-api:test",
		PayloadProcessingImage:       "quay.io/example/payload:test",
		MaaSAPIKeyCleanupImage:       "quay.io/example/cleanup:test",
		APIKeyMaxExpirationDays:      "45",
		PayloadProcessingAutoscaling: true,
	}
	operands, err := tenantreconcile.PostRender(context.Background(), logr.Discard(), tenant, rendered, params)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(operands).NotTo(BeEmpty())
	return operands
}
