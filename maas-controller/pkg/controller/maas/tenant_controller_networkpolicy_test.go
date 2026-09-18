package maas

import (
	"context"
	"testing"

	netwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
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
	)

	r := &TenantReconciler{
		AppNamespace:                    infraNS,
		GatewayNamespace:                gatewayNS,
		TenantNamespace:                 tenantNS,
		TenantNamespaceDiscoveryEnabled: true,
	}

	np := &netwv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "maas-api-allow-gateway",
			Namespace: infraNS,
			Labels: map[string]string{
				tenantreconcile.LabelODHAppPrefix + "/" + tenantreconcile.ComponentName: "true",
				tenantreconcile.LabelTenantNamespace:                                  tenantNS,
			},
		},
	}

	reqs := r.mapNetworkPolicyToMaasTenantConfigs(context.Background(), np)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 reconcile request, got %d", len(reqs))
	}
	want := types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}
	if reqs[0].NamespacedName != want {
		t.Fatalf("unexpected request: got %v want %v", reqs[0].NamespacedName, want)
	}

	foreign := np.DeepCopy()
	foreign.Namespace = "other"
	if len(r.mapNetworkPolicyToMaasTenantConfigs(context.Background(), foreign)) != 0 {
		t.Fatal("expected foreign namespace NetworkPolicy to be ignored")
	}
}
