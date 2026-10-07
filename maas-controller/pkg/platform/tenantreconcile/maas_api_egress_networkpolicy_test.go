package tenantreconcile

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func sampleMaaSAPIEgressRestrictNetworkPolicy() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata": map[string]any{
				"name": baseMaaSAPIEgressRestrictNetworkPolicyName,
			},
			"spec": map[string]any{
				"egress": []any{
					map[string]any{
						"ports": []any{
							map[string]any{"port": int64(53), "protocol": "UDP"},
						},
					},
					map[string]any{
						"ports": []any{
							map[string]any{"port": int64(443), "protocol": "TCP"},
						},
					},
				},
			},
		},
	}
}

func TestPatchMaaSAPIEgressRestrict_preservesLiveEgressOnUpgrade(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	preserved := []any{
		map[string]any{
			"to": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "postgres"}}}},
			"ports": []any{map[string]any{"port": int64(5432), "protocol": "TCP"}},
		},
	}
	params := PlatformParams{
		MaaSAPIPreserveEgressOnUpgrade: true,
		MaaSAPIPreservedEgressRules:    preserved,
	}
	require.NoError(t, patchMaaSAPIEgressRestrictNetworkPolicy(np, params))

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 1)
	assert.False(t, hasAllowAllEgressRule(egress))
	rule, ok := egress[0].(map[string]any)
	require.True(t, ok)
	_, hasTo := rule["to"]
	assert.True(t, hasTo)
}

func TestPatchMaaSAPIEgressRestrict_addsAllowAllByDefault(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	require.NoError(t, patchMaaSAPIEgressRestrictNetworkPolicy(np, PlatformParams{}))

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 3)
	assert.Empty(t, egress[2])
}

func TestPatchMaaSAPIEgressRestrict_replacesWithCustomRules(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	tcp := corev1.ProtocolTCP
	params := PlatformParams{
		MaaSAPIEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}},
				},
				To: []netwv1.NetworkPolicyPeer{
					{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "my-postgres"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, patchMaaSAPIEgressRestrictNetworkPolicy(np, params))

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 1)
	rule, ok := egress[0].(map[string]any)
	require.True(t, ok)
	ports, ok := rule["ports"].([]any)
	require.True(t, ok)
	assert.Len(t, ports, 1)
}

func TestPatchMaaSAPIEgressRestrict_appendsAdditionalRules(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	tcp := corev1.ProtocolTCP
	params := PlatformParams{
		MaaSAPIAdditionalEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 15432}},
				},
			},
		},
	}
	require.NoError(t, patchMaaSAPIEgressRestrictNetworkPolicy(np, params))

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 4)
	rule, ok := egress[3].(map[string]any)
	require.True(t, ok)
	ports, ok := rule["ports"].([]any)
	require.True(t, ok)
	assert.Len(t, ports, 1)
}

func TestPatchMaaSAPIEgressRestrict_customRulesWithAdditional(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	tcp := corev1.ProtocolTCP
	params := PlatformParams{
		MaaSAPIEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 443}},
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 6443}},
				},
			},
		},
		MaaSAPIAdditionalEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}},
				},
				To: []netwv1.NetworkPolicyPeer{
					{
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "postgres"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, patchMaaSAPIEgressRestrictNetworkPolicy(np, params))

	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, egress, 2)
}

func TestValidateRestrictedEgressRules(t *testing.T) {
	tcp := corev1.ProtocolTCP
	apiRule := netwv1.NetworkPolicyEgressRule{
		Ports: []netwv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 443}},
			{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 6443}},
		},
	}
	completeRule := netwv1.NetworkPolicyEgressRule{
		Ports: []netwv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}},
		},
		To: []netwv1.NetworkPolicyPeer{
			{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}}},
		},
	}

	assert.True(t, isKubernetesAPIPortOnlyEgressRule(apiRule))
	require.NoError(t, validateRestrictedEgressRules([]netwv1.NetworkPolicyEgressRule{apiRule, completeRule}))

	err := validateRestrictedEgressRules([]netwv1.NetworkPolicyEgressRule{
		{Ports: []netwv1.NetworkPolicyPort{{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 22}}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destination")

	err = validateRestrictedEgressRules([]netwv1.NetworkPolicyEgressRule{
		{To: []netwv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port")
}

func TestPatchMaaSAPIEgressRestrict_rejectsIncompleteCustomRules(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	tcp := corev1.ProtocolTCP
	params := PlatformParams{
		MaaSAPIEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}},
				},
			},
		},
	}
	err := patchMaaSAPIEgressRestrictNetworkPolicy(np, params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate custom maas-api egress rules")
}

func TestPatchMaaSAPIEgressRestrict_rejectsIncompleteAdditionalWhenRestrictive(t *testing.T) {
	np := sampleMaaSAPIEgressRestrictNetworkPolicy()
	tcp := corev1.ProtocolTCP
	params := PlatformParams{
		MaaSAPIEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 443}},
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 6443}},
				},
			},
		},
		MaaSAPIAdditionalEgressRules: []netwv1.NetworkPolicyEgressRule{
			{
				Ports: []netwv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}},
				},
			},
		},
	}
	err := patchMaaSAPIEgressRestrictNetworkPolicy(np, params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate additional maas-api egress rules")
}

func TestApplyMaaSAPIPreserveEgressConfig(t *testing.T) {
	const appNs = "maas-system"
	configUID := types.UID("cfg-uid")
	controller := true

	t.Run("no-op when explicit networkPolicyEgressRules set", func(t *testing.T) {
		tcp := corev1.ProtocolTCP
		mcfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: configUID},
			Spec: maasv1alpha1.ConfigSpec{
				NetworkPolicyEgressRules: []netwv1.NetworkPolicyEgressRule{
					{
						Ports: []netwv1.NetworkPolicyPort{{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}}},
						To:    []netwv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}}}},
					},
				},
			},
		}
		params := PlatformParams{}
		applyMaaSAPIEgressConfig(&params, mcfg)
		cl := fake.NewClientBuilder().Build()
		require.NoError(t, applyMaaSAPIPreserveEgressConfig(context.Background(), cl, &params, mcfg, appNs))
		assert.False(t, params.MaaSAPIPreserveEgressOnUpgrade)
	})

	t.Run("preserves restrictive live policy without allow-all", func(t *testing.T) {
		mcfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: configUID},
		}
		live := sampleMaaSAPIEgressRestrictNetworkPolicy()
		live.SetNamespace(appNs)
		live.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        configUID,
			Controller: &controller,
		}})
		_ = unstructured.SetNestedSlice(live.Object, []any{
			map[string]any{"ports": []any{map[string]any{"port": int64(443), "protocol": "TCP"}}},
			map[string]any{
				"to":    []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "postgres"}}}},
				"ports": []any{map[string]any{"port": int64(5432), "protocol": "TCP"}},
			},
		}, "spec", "egress")

		params := PlatformParams{}
		applyMaaSAPIEgressConfig(&params, mcfg)
		cl := fake.NewClientBuilder().WithObjects(live).Build()
		require.NoError(t, applyMaaSAPIPreserveEgressConfig(context.Background(), cl, &params, mcfg, appNs))
		assert.True(t, params.MaaSAPIPreserveEgressOnUpgrade)
		require.Len(t, params.MaaSAPIPreservedEgressRules, 2)
		assert.False(t, hasAllowAllEgressRule(params.MaaSAPIPreservedEgressRules))
	})

	t.Run("skips when live policy already has allow-all", func(t *testing.T) {
		mcfg := &maasv1alpha1.Config{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: configUID},
		}
		live := sampleMaaSAPIEgressRestrictNetworkPolicy()
		live.SetNamespace(appNs)
		setConfigControllerOwnerRef(live, configUID)
		_ = unstructured.SetNestedSlice(live.Object, []any{
			map[string]any{"ports": []any{map[string]any{"port": int64(443), "protocol": "TCP"}}},
			map[string]any{},
		}, "spec", "egress")

		params := PlatformParams{}
		applyMaaSAPIEgressConfig(&params, mcfg)
		cl := fake.NewClientBuilder().WithObjects(live).Build()
		require.NoError(t, applyMaaSAPIPreserveEgressConfig(context.Background(), cl, &params, mcfg, appNs))
		assert.False(t, params.MaaSAPIPreserveEgressOnUpgrade)
	})
}

func TestRemoveAllowAllEgressRules(t *testing.T) {
	egress := []any{
		map[string]any{"ports": []any{map[string]any{"port": int64(53)}}},
		map[string]any{},
		map[string]any{"to": []any{map[string]any{}}},
	}
	filtered := removeAllowAllEgressRules(egress)
	require.Len(t, filtered, 2)
}

func TestApplyMaaSAPIEgressConfig(t *testing.T) {
	tcp := corev1.ProtocolTCP
	customRule := []netwv1.NetworkPolicyEgressRule{
		{Ports: []netwv1.NetworkPolicyPort{{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 5432}}}},
	}
	additionalRule := []netwv1.NetworkPolicyEgressRule{
		{Ports: []netwv1.NetworkPolicyPort{{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 15432}}}},
	}

	t.Run("nil config is a no-op", func(t *testing.T) {
		params := PlatformParams{MaaSAPIEgressRules: customRule}
		applyMaaSAPIEgressConfig(&params, nil)
		assert.Equal(t, customRule, params.MaaSAPIEgressRules)
		assert.False(t, params.MaaSAPIEgressNetworkPolicyDisabled)
	})

	t.Run("managed copies egress rules", func(t *testing.T) {
		params := PlatformParams{}
		mcfg := &maasv1alpha1.Config{
			Spec: maasv1alpha1.ConfigSpec{
				MaasAPIEgressNetworkPolicy:         maasv1alpha1.MaaSAPIEgressNetworkPolicyManaged,
				NetworkPolicyEgressRules:           customRule,
				NetworkPolicyAdditionalEgressRules: additionalRule,
			},
		}
		applyMaaSAPIEgressConfig(&params, mcfg)
		assert.False(t, params.MaaSAPIEgressNetworkPolicyDisabled)
		assert.Equal(t, customRule, params.MaaSAPIEgressRules)
		assert.Equal(t, additionalRule, params.MaaSAPIAdditionalEgressRules)
	})

	t.Run("disabled clears egress rules", func(t *testing.T) {
		params := PlatformParams{MaaSAPIEgressRules: customRule, MaaSAPIAdditionalEgressRules: additionalRule}
		mcfg := &maasv1alpha1.Config{
			Spec: maasv1alpha1.ConfigSpec{
				MaasAPIEgressNetworkPolicy: maasv1alpha1.MaaSAPIEgressNetworkPolicyDisabled,
			},
		}
		applyMaaSAPIEgressConfig(&params, mcfg)
		assert.True(t, params.MaaSAPIEgressNetworkPolicyDisabled)
		assert.Nil(t, params.MaaSAPIEgressRules)
		assert.Nil(t, params.MaaSAPIAdditionalEgressRules)
	})
}

func TestIsMaaSManagedMaaSAPIEgressNetworkPolicy(t *testing.T) {
	const configUID = types.UID("cfg-uid")
	otherUID := types.UID("other-uid")
	controller := true

	owned := sampleMaaSAPIEgressRestrictNetworkPolicy()
	setConfigControllerOwnerRef(owned, configUID)

	ssaManaged := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"managedFields": []any{map[string]any{"manager": ssaFieldOwner}},
		},
	}}
	ssaManaged.SetGroupVersionKind(GVKNetworkPolicy)

	external := sampleMaaSAPIEgressRestrictNetworkPolicy()
	external.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "other",
		UID:        otherUID,
		Controller: &controller,
	}})

	unrelated := sampleMaaSAPIEgressRestrictNetworkPolicy()

	assert.True(t, isMaaSManagedMaaSAPIEgressNetworkPolicy(owned, configUID))
	assert.True(t, isMaaSManagedMaaSAPIEgressNetworkPolicy(ssaManaged, configUID))
	assert.False(t, isMaaSManagedMaaSAPIEgressNetworkPolicy(external, configUID))
	assert.False(t, isMaaSManagedMaaSAPIEgressNetworkPolicy(unrelated, configUID))
}

func TestMaasAPIEgressNetworkPolicyDisabled(t *testing.T) {
	assert.False(t, maasAPIEgressNetworkPolicyDisabled(maasv1alpha1.ConfigSpec{}))
	assert.False(t, maasAPIEgressNetworkPolicyDisabled(maasv1alpha1.ConfigSpec{
		MaasAPIEgressNetworkPolicy: maasv1alpha1.MaaSAPIEgressNetworkPolicyManaged,
	}))
	assert.True(t, maasAPIEgressNetworkPolicyDisabled(maasv1alpha1.ConfigSpec{
		MaasAPIEgressNetworkPolicy: maasv1alpha1.MaaSAPIEgressNetworkPolicyDisabled,
	}))
}

func TestPostRender_SkipsMaaSAPIEgressWhenDisabled(t *testing.T) {
	rendered := []unstructured.Unstructured{
		*sampleMaaSAPIEgressRestrictNetworkPolicy(),
		{
			Object: map[string]any{
				"apiVersion": "networking.k8s.io/v1",
				"kind":       "NetworkPolicy",
				"metadata": map[string]any{
					"name": "maas-api-allow-gateway",
				},
			},
		},
	}
	params := PlatformParams{MaaSAPIEgressNetworkPolicyDisabled: true}

	resources, err := PostRender(context.Background(), logr.Discard(), nil, rendered, params)
	require.NoError(t, err)

	for _, r := range resources {
		if r.GroupVersionKind() == GVKNetworkPolicy && r.GetName() == baseMaaSAPIEgressRestrictNetworkPolicyName {
			t.Fatalf("unexpected maas-api egress restrict policy in output")
		}
	}
	requireResourceName(t, resources, GVKNetworkPolicy, "maas-api-allow-gateway")
}

func TestCleanupMaaSAPIEgressNetworkPolicy(t *testing.T) {
	const appNs = "maas-system"
	configUID := types.UID("config-uid")

	t.Run("no-op when policy is absent", func(t *testing.T) {
		cl := fake.NewClientBuilder().Build()
		err := cleanupMaaSAPIEgressNetworkPolicy(context.Background(), cl, appNs, configUID, logr.Discard())
		require.NoError(t, err)
	})

	t.Run("skips policy not managed by maas-controller", func(t *testing.T) {
		np := sampleMaaSAPIEgressRestrictNetworkPolicy()
		np.SetNamespace(appNs)
		np.SetUID("np-uid")
		np.SetResourceVersion("1")
		cl := fake.NewClientBuilder().WithObjects(np).Build()

		err := cleanupMaaSAPIEgressNetworkPolicy(context.Background(), cl, appNs, configUID, logr.Discard())
		require.NoError(t, err)

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(GVKNetworkPolicy)
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: appNs, Name: baseMaaSAPIEgressRestrictNetworkPolicyName}, got))
	})

	t.Run("deletes Config-owned policy", func(t *testing.T) {
		controller := true
		np := sampleMaaSAPIEgressRestrictNetworkPolicy()
		np.SetNamespace(appNs)
		np.SetUID("np-uid")
		np.SetResourceVersion("1")
		np.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "maas.opendatahub.io/v1alpha1",
			Kind:       "Config",
			Name:       maasv1alpha1.ConfigInstanceName,
			UID:        configUID,
			Controller: &controller,
		}})
		cl := fake.NewClientBuilder().WithObjects(np).Build()

		err := cleanupMaaSAPIEgressNetworkPolicy(context.Background(), cl, appNs, configUID, logr.Discard())
		require.NoError(t, err)

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(GVKNetworkPolicy)
		err = cl.Get(context.Background(), types.NamespacedName{Namespace: appNs, Name: baseMaaSAPIEgressRestrictNetworkPolicyName}, got)
		require.Error(t, err)
	})
}

func TestPostRender_maasAPIEgressIncludesAllowAllByDefault(t *testing.T) {
	const appNs = "odh-ai-gateway-infra"
	rendered := renderOverlayResources(t, appNs)
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: appNs,
		},
	}
	params := PlatformParams{
		AppNamespace:        appNs,
		ControllerNamespace: "controller-ns",
		GatewayNamespace:    "openshift-ingress",
		GatewayName:         "gateway",
		ClusterAudience:     "openshift",
		SkipIPP:             true,
	}

	resources, err := PostRender(context.Background(), logr.Discard(), tenant, rendered, params)
	require.NoError(t, err)

	np := requireResourceName(t, resources, GVKNetworkPolicy, baseMaaSAPIEgressRestrictNetworkPolicyName)
	egress, found, err := unstructured.NestedSlice(np.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, egress)
	lastRule, ok := egress[len(egress)-1].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, lastRule, "default egress should end with allow-all")
}

func TestRunPlatform_DisabledOmitsAndDeletesMaaSAPIEgressNetworkPolicy(t *testing.T) {
	const (
		tenantName = "praxis-team"
		appNs      = "ai-tenant-praxis-team"
		gwNS       = "openshift-ingress"
		gwName     = "praxis-gateway"
	)
	scheme := praxisTestScheme(t)
	tenant := praxisTenantConfig(appNs, tenantName)
	mcfg := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: types.UID("cfg-uid")},
		Spec: maasv1alpha1.ConfigSpec{
			MaasAPIEgressNetworkPolicy: maasv1alpha1.MaaSAPIEgressNetworkPolicyDisabled,
		},
	}
	gateway := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS},
	}
	existingNP := sampleMaaSAPIEgressRestrictNetworkPolicy()
	existingNP.SetNamespace(appNs)
	existingNP.SetUID("np-uid")
	existingNP.SetResourceVersion("1")
	setConfigControllerOwnerRef(existingNP, mcfg.UID)

	platformContext := PlatformContext{
		GatewayRef: maasv1alpha1.TenantGatewayRef{Namespace: gwNS, Name: gwName},
		SkipIPP:    true,
		Source:     "aitenant",
	}

	var applied []appliedResource
	cl := runPlatformTestClient(t, scheme, []client.Object{
		mcfg, gateway, tenant, readyMaaSAPIDeployment(appNs, tenantName), existingNP,
	}, &applied)

	result, err := RunPlatform(
		context.Background(),
		logr.Discard(),
		cl,
		scheme,
		tenant,
		platformContext,
		platformOverlayManifestPath(t),
		appNs,
		"controller-ns",
		"https://kubernetes.default.svc",
		"opendatahub",
		mcfg,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.DeploymentPending, result.Detail)

	for _, res := range applied {
		if res.gvk == GVKNetworkPolicy && res.name == baseMaaSAPIEgressRestrictNetworkPolicyName {
			t.Fatalf("unexpected maas-api egress restrict policy applied when Disabled")
		}
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GVKNetworkPolicy)
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: appNs, Name: baseMaaSAPIEgressRestrictNetworkPolicyName}, got)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}

func requireResourceName(t *testing.T, resources []unstructured.Unstructured, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	for i := range resources {
		if resources[i].GroupVersionKind() == gvk && resources[i].GetName() == name {
			return &resources[i]
		}
	}
	t.Fatalf("resource %s %s not found in output", gvk.Kind, name)
	return nil
}
