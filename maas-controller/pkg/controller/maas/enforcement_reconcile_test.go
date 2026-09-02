/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// withExactPath gives a route a single Exact path-match rule.
func withExactPath(rt *gatewayapiv1.HTTPRoute, path string) *gatewayapiv1.HTTPRoute {
	pt := gatewayapiv1.PathMatchExact
	rt.Spec.Rules = []gatewayapiv1.HTTPRouteRule{{
		Matches: []gatewayapiv1.HTTPRouteMatch{{Path: &gatewayapiv1.HTTPPathMatch{Type: &pt, Value: &path}}},
	}}
	return rt
}

// TestGatherAndGenerateForGateway covers gather, compile, render, and ConfigMap
// build. The fake client cannot exercise server-side apply, so the skip test
// covers Reconcile's no-requeue path.
func TestGatherAndGenerateForGateway(t *testing.T) {
	const gwNS, gwName = "openshift-ingress", "maas-site-a-gateway"
	gw := newGatewayWithHostname(gwName, gwNS, "site.example.com")
	modelRoute := withExactPath(newHTTPRouteWithGateway("maas-foo", "default", gwName, gwNS), "/v1/chat/completions")
	apiRoute := withExactPath(newHTTPRouteWithGateway("maas-api-route-site-a", "default", gwName, gwNS), "/maas-api/v1/models")
	model := newMaaSModelRef("foo", "default", "ExternalModel", "foo")
	sub := newMaaSSubscription("sub-a", "default", "group-a", "foo", 1000)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(gw, modelRoute, apiRoute, model, sub).
		Build()
	r := &GatewayEnforcementReconciler{
		Client:           c,
		Scheme:           scheme,
		InfraNamespace:   "opendatahub",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
	}

	in, err := r.gatherGatewayInput(context.Background(), types.NamespacedName{Namespace: gwNS, Name: gwName})
	if err != nil {
		t.Fatalf("gatherGatewayInput: %v", err)
	}
	if in.Hostname != "site.example.com" {
		t.Errorf("hostname = %q, want site.example.com", in.Hostname)
	}
	if len(in.Subscriptions) != 1 || in.Subscriptions[0].MaxValue != 1000 {
		t.Errorf("subscriptions = %+v, want one with limit 1000", in.Subscriptions)
	}

	arts, err := generateGatewayArtifacts(in)
	if err != nil {
		t.Fatalf("generateGatewayArtifacts: %v", err)
	}
	if len(arts.PluginConfig.ActionSets) == 0 {
		t.Error("no action sets generated")
	}
	if len(arts.Limits) != 2 { // one subscription limit + the deny-all
		t.Errorf("limits = %d, want 2 (subscription + deny-all)", len(arts.Limits))
	}

	rendered, err := r.renderPluginConfig(arts.PluginConfig)
	if err != nil {
		t.Fatalf("renderPluginConfig: %v", err)
	}
	cm := configMapFor(gwName, gwNS, rendered)
	if cm.Name != gwName+"-extproc" || !strings.Contains(cm.Data["kuadrant.yaml"], "actionSets") {
		t.Errorf("configMap = %+v", cm)
	}
}

func TestReconcileSkipsNotReadyGateway(t *testing.T) {
	const gwNS, gwName = "openshift-ingress", "maas-x-gateway"
	cases := []struct {
		name string
		objs []client.Object
	}{
		{"no hostname", []client.Object{&gatewayapiv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNS}}}},
		{"no routes", []client.Object{newGatewayWithHostname(gwName, gwNS, "x.example.com")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(testRESTMapper()).WithObjects(tc.objs...).Build()
			r := &GatewayEnforcementReconciler{Client: c, Scheme: scheme, InfraNamespace: "opendatahub"}
			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gwNS, Name: gwName}})
			if err != nil {
				t.Fatalf("Reconcile should skip, got error: %v", err)
			}
			if res != (reconcile.Result{}) {
				t.Errorf("result = %+v, want no requeue", res)
			}
			cm := &corev1.ConfigMap{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: gwNS, Name: gwName + "-extproc"}, cm); !apierrors.IsNotFound(err) {
				t.Errorf("expected no ConfigMap, got err=%v", err)
			}
		})
	}
}

func TestGatewaysForRoute(t *testing.T) {
	r := &GatewayEnforcementReconciler{}
	rt := newHTTPRouteWithGateway("m", "default", "gw-1", "openshift-ingress")
	got := r.gatewaysForRoute(context.Background(), rt)
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "openshift-ingress", Name: "gw-1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gatewaysForRoute = %v, want %v", got, want)
	}
}

func TestGatewaysForSubscription(t *testing.T) {
	gw1 := newGatewayWithHostname("gw-1", "ns", "h1")
	gw1.Labels = map[string]string{aiGatewayTenantLabel: "t1"}
	gw2 := newGatewayWithHostname("gw-2", "ns", "h2") // unlabeled: must be ignored

	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(testRESTMapper()).WithObjects(gw1, gw2).Build()
	r := &GatewayEnforcementReconciler{Client: c}
	got := r.gatewaysForSubscription(context.Background(), &maasv1alpha1.MaaSSubscription{})
	if len(got) != 1 || got[0].Name != "gw-1" {
		t.Errorf("gatewaysForSubscription = %v, want only gw-1", got)
	}
}
