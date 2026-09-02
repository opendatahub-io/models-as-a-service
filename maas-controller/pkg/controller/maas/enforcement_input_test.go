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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestHTTPRouteMatches(t *testing.T) {
	exact := gatewayapiv1.PathMatchExact
	prefix := gatewayapiv1.PathMatchPathPrefix
	p1, p2 := "/v1/chat/completions", "/v1"
	rt := &gatewayapiv1.HTTPRoute{
		Spec: gatewayapiv1.HTTPRouteSpec{Rules: []gatewayapiv1.HTTPRouteRule{
			{Matches: []gatewayapiv1.HTTPRouteMatch{
				{Path: &gatewayapiv1.HTTPPathMatch{Type: &exact, Value: &p1}, Headers: []gatewayapiv1.HTTPHeaderMatch{{Name: "X-Model", Value: "m"}}},
				{Path: &gatewayapiv1.HTTPPathMatch{Type: &prefix, Value: &p2}},
			}},
		}},
	}
	got := httpRouteMatches(rt)
	want := []RouteMatch{
		{PathType: "Exact", PathValue: p1, HeaderName: "X-Model", HeaderValue: "m"},
		{PathType: "PathPrefix", PathValue: p2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("httpRouteMatches = %#v, want %#v", got, want)
	}
}

func TestRouteTargetsGateway(t *testing.T) {
	gw := types.NamespacedName{Namespace: "openshift-ingress", Name: "maas-gw"}
	if !routeTargetsGateway(newHTTPRouteWithGateway("r", "default", "maas-gw", "openshift-ingress"), gw) {
		t.Error("route parent-refs the gateway but was not matched")
	}
	if routeTargetsGateway(newHTTPRouteWithGateway("r", "default", "other-gw", "openshift-ingress"), gw) {
		t.Error("route targeting a different gateway name was matched")
	}
	if routeTargetsGateway(newHTTPRouteWithGateway("r", "default", "maas-gw", "other-ns"), gw) {
		t.Error("route targeting the gateway in a different namespace was matched")
	}
}
