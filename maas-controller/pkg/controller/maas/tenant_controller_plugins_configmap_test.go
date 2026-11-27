package maas

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const ippProcessingConfigKey = "custom-ipp-config.yaml"

func pluginsConfigMap(namespace, name string, labels map[string]string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       data,
	}
}

func TestMapPluginsConfigMapToMaasTenantConfigs(t *testing.T) {
	const (
		infraNS   = "redhat-ai-gateway-infra"
		gatewayNS = "openshift-ingress"
		defaultNS = "models-as-a-service"
		tenantNS  = "ai-tenant-team-a"
	)
	ipp := map[string]string{ippProcessingConfigKey: "profiles: []"}
	praxis := map[string]string{"extproc.yaml": "{}"}
	tenantLabels := map[string]string{tenantreconcile.LabelTenantNamespace: tenantNS}
	defaultTenant := types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: defaultNS}
	labeledTenant := types.NamespacedName{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNS}

	tests := []struct {
		name      string
		discovery bool
		cm        *corev1.ConfigMap
		want      []types.NamespacedName
	}{
		{
			name: "default tenant ConfigMap",
			cm:   pluginsConfigMap(gatewayNS, tenantreconcile.PayloadProcessingPluginsConfigMapName, nil, ipp),
			want: []types.NamespacedName{defaultTenant},
		},
		{
			name:      "per-tenant ConfigMap with discovery",
			discovery: true,
			cm:        pluginsConfigMap(gatewayNS, tenantreconcile.PayloadProcessingPluginsConfigMapForTenant("team-a"), tenantLabels, ipp),
			want:      []types.NamespacedName{labeledTenant},
		},
		{
			name:      "unlabeled ConfigMap with discovery",
			discovery: true,
			cm:        pluginsConfigMap(gatewayNS, tenantreconcile.PayloadProcessingPluginsConfigMapName, nil, ipp),
			want:      []types.NamespacedName{defaultTenant},
		},
		{
			name: "praxis ConfigMap under the same name",
			cm:   pluginsConfigMap(gatewayNS, tenantreconcile.PayloadProcessingPluginsConfigMapName, tenantLabels, praxis),
		},
		{
			name: "other ConfigMap",
			cm:   pluginsConfigMap(gatewayNS, "payload-processing-settings", tenantLabels, ipp),
		},
		{
			name: "foreign namespace",
			cm:   pluginsConfigMap("other", tenantreconcile.PayloadProcessingPluginsConfigMapName, tenantLabels, ipp),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &TenantReconciler{
				AppNamespace:                    infraNS,
				GatewayNamespace:                gatewayNS,
				TenantNamespace:                 defaultNS,
				TenantNamespaceDiscoveryEnabled: tt.discovery,
			}
			var got []types.NamespacedName
			for _, req := range r.mapPluginsConfigMapToMaasTenantConfigs(t.Context(), tt.cm) {
				got = append(got, req.NamespacedName)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIPPPluginsConfigMapChanged(t *testing.T) {
	const gatewayNS = "openshift-ingress"
	name := tenantreconcile.PayloadProcessingPluginsConfigMapForTenant("team-a")
	ipp := pluginsConfigMap(gatewayNS, name, nil, map[string]string{ippProcessingConfigKey: "profiles: []"})
	praxis := pluginsConfigMap(gatewayNS, name, nil, map[string]string{"extproc.yaml": "{}"})
	other := pluginsConfigMap(gatewayNS, "unrelated", nil, map[string]string{ippProcessingConfigKey: "profiles: []"})
	p := ippPluginsConfigMapChanged()

	assert.True(t, p.Create(event.CreateEvent{Object: ipp}), "create of an IPP plugins ConfigMap")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ipp, ObjectNew: ipp}), "edit of an IPP plugins ConfigMap")
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: ipp, ObjectNew: praxis}), "IPP config removed")
	assert.True(t, p.Delete(event.DeleteEvent{Object: ipp}), "delete of an IPP plugins ConfigMap")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: praxis, ObjectNew: praxis}), "praxis ConfigMap under the same name")
	assert.False(t, p.Create(event.CreateEvent{Object: other}), "ConfigMap with another name")
}
