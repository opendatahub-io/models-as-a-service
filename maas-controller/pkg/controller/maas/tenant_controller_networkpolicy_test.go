package maas

import (
	"testing"

	netwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func TestIsManagedTenantNetworkPolicyLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "odh component label",
			labels: map[string]string{
				tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName: "true",
			},
			want: true,
		},
		{
			name: "maas part-of label",
			labels: map[string]string{
				"app.kubernetes.io/part-of": "maas",
			},
			want: true,
		},
		{
			name: "tenant tracking labels",
			labels: map[string]string{
				tenantreconcile.LabelTenantNamespace: "tenant-a",
			},
			want: true,
		},
		{
			name:   "unrelated",
			labels: map[string]string{"app": "other"},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isManagedTenantNetworkPolicyLabels(tt.labels); got != tt.want {
				t.Fatalf("isManagedTenantNetworkPolicyLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapNetworkPolicyToMaasTenantConfigs(t *testing.T) {
	const (
		infraNS   = "redhat-ai-gateway-infra"
		gatewayNS = "openshift-ingress"
		tenantNS  = "models-as-a-service"
		teamANS   = "ai-tenant-team-a"
	)

	tenantRequest := func(namespace string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace}}
	}
	tenantConfig := func(namespace string) *maasv1alpha1.MaasTenantConfig {
		return &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace}}
	}
	r := &TenantReconciler{
		Client:                          fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantConfig(tenantNS), tenantConfig(teamANS)).Build(),
		AppNamespace:                    infraNS,
		GatewayNamespace:                gatewayNS,
		TenantNamespace:                 tenantNS,
		TenantNamespaceDiscoveryEnabled: true,
	}
	networkPolicy := func(name, namespace, trackedTenant string, shared bool) *netwv1.NetworkPolicy {
		l := map[string]string{
			tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName: "true",
		}
		if shared {
			l[tenantreconcile.LabelSharedOperand] = "true"
		} else {
			l[tenantreconcile.LabelTenantName] = trackedTenant
			l[tenantreconcile.LabelTenantNamespace] = "ai-tenant-" + trackedTenant
		}
		return &netwv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: l}}
	}

	tests := []struct {
		name string
		np   *netwv1.NetworkPolicy
		want []reconcile.Request
	}{
		{
			// The default tenant config (tenantNS) repairs it: every tenant renders a
			// shared policy identically, so one live, reconcilable tenant is enough.
			name: "shared policy in the app namespace enqueues the default tenant",
			np:   networkPolicy("maas-api-allow-gateway", infraNS, "", true),
			want: []reconcile.Request{tenantRequest(tenantNS)},
		},
		{
			name: "per-tenant policy in the gateway namespace enqueues its tenant",
			np:   networkPolicy(tenantreconcile.PayloadProcessingNetworkPolicyName("team-a"), gatewayNS, "team-a", false),
			want: []reconcile.Request{tenantRequest(teamANS)},
		},
		{
			// A relabel is recovered by the mapped handler invoking this mapper on both
			// the old and the new object on Update (controller-runtime), not by inference
			// in a single call here: this call alone maps by whatever labels the object
			// carries now, even when they no longer name its real owner.
			name: "per-tenant policy under another tenant's labels maps by those labels alone",
			np:   networkPolicy(tenantreconcile.PayloadProcessingNetworkPolicyName("team-a"), gatewayNS, "deleted", false),
			want: []reconcile.Request{tenantRequest("ai-tenant-deleted")},
		},
		{
			name: "policy outside the platform namespaces is ignored",
			np:   networkPolicy("maas-api-allow-gateway", "other", "team-a", false),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NewWithT(t).Expect(r.mapNetworkPolicyToMaasTenantConfigs(t.Context(), tt.np)).To(ConsistOf(tt.want))
		})
	}
}
