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
	networkPolicy := func(name, namespace, trackedTenant string) *netwv1.NetworkPolicy {
		return &netwv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName: "true",
				tenantreconcile.LabelTenantName:                                         trackedTenant,
				tenantreconcile.LabelTenantNamespace:                                    "ai-tenant-" + trackedTenant,
			},
		}}
	}

	tests := []struct {
		name string
		np   *netwv1.NetworkPolicy
		want []reconcile.Request
	}{
		{
			// The labels name the tenant that applied last, which may be gone.
			name: "shared policy in the app namespace enqueues every tenant",
			np:   networkPolicy("maas-api-allow-gateway", infraNS, "deleted"),
			want: []reconcile.Request{tenantRequest(tenantNS), tenantRequest(teamANS)},
		},
		{
			name: "per-tenant policy in the gateway namespace enqueues its tenant",
			np:   networkPolicy(tenantreconcile.PayloadProcessingNetworkPolicyName("team-a"), gatewayNS, "team-a"),
			want: []reconcile.Request{tenantRequest(teamANS)},
		},
		{
			// Its owner is the only tenant that renders it, and the labels no longer say so.
			name: "per-tenant policy under another tenant's labels enqueues every tenant",
			np:   networkPolicy(tenantreconcile.PayloadProcessingNetworkPolicyName("team-a"), gatewayNS, "deleted"),
			want: []reconcile.Request{tenantRequest(tenantNS), tenantRequest(teamANS)},
		},
		{
			name: "policy outside the platform namespaces is ignored",
			np:   networkPolicy("maas-api-allow-gateway", "other", "team-a"),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NewWithT(t).Expect(r.mapNetworkPolicyToMaasTenantConfigs(t.Context(), tt.np)).To(ConsistOf(tt.want))
		})
	}
}
