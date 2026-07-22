package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestSetHTTPRouteOwnerRef_NonTelemetryPolicy(t *testing.T) {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata":   map[string]any{"name": "test", "namespace": "ns"},
		},
	}
	routes := map[string]metav1.OwnerReference{
		"ns/test": {APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute", Name: "test", UID: "abc"},
	}
	setHTTPRouteOwnerRef(u, routes)
	assert.Empty(t, u.GetOwnerReferences())
}

func TestSetHTTPRouteOwnerRef_GatewayTargetUnchanged(t *testing.T) {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata":   map[string]any{"name": "maas-telemetry", "namespace": "openshift-ingress"},
			"spec": map[string]any{
				"targetRef": map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "Gateway",
					"name":  "maas-default-gateway",
				},
			},
		},
	}
	routes := map[string]metav1.OwnerReference{
		"openshift-ingress/maas-api-route": {
			APIVersion: "gateway.networking.k8s.io/v1",
			Kind:       "HTTPRoute",
			Name:       "maas-api-route",
			UID:        "route-uid",
		},
	}
	setHTTPRouteOwnerRef(u, routes)
	assert.Empty(t, u.GetOwnerReferences())
}

func TestSetHTTPRouteOwnerRef_SetsOwnerForHTTPRouteTarget(t *testing.T) {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata":   map[string]any{"name": "maas-mgmt-telemetry", "namespace": "opendatahub"},
			"spec": map[string]any{
				"targetRef": map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "HTTPRoute",
					"name":  "maas-api-route",
				},
			},
		},
	}
	routeUID := types.UID("httproute-uid-123")
	routes := map[string]metav1.OwnerReference{
		"opendatahub/maas-api-route": {
			APIVersion: "gateway.networking.k8s.io/v1",
			Kind:       "HTTPRoute",
			Name:       "maas-api-route",
			UID:        routeUID,
		},
	}
	setHTTPRouteOwnerRef(u, routes)

	refs := u.GetOwnerReferences()
	assert.Len(t, refs, 1)
	assert.Equal(t, "maas-api-route", refs[0].Name)
	assert.Equal(t, routeUID, refs[0].UID)
	assert.Equal(t, "HTTPRoute", refs[0].Kind)
	assert.Nil(t, refs[0].Controller, "must be a non-controller ownerReference")
}

func TestSetHTTPRouteOwnerRef_NoMatchingRoute(t *testing.T) {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata":   map[string]any{"name": "maas-mgmt-telemetry", "namespace": "opendatahub"},
			"spec": map[string]any{
				"targetRef": map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "HTTPRoute",
					"name":  "maas-api-route",
				},
			},
		},
	}
	routes := map[string]metav1.OwnerReference{}
	setHTTPRouteOwnerRef(u, routes)
	assert.Empty(t, u.GetOwnerReferences())
}

func TestSetHTTPRouteOwnerRef_Idempotent(t *testing.T) {
	routeUID := types.UID("httproute-uid-456")
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata":   map[string]any{"name": "maas-mgmt-telemetry", "namespace": "opendatahub"},
			"spec": map[string]any{
				"targetRef": map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "HTTPRoute",
					"name":  "maas-api-route",
				},
			},
		},
	}
	routes := map[string]metav1.OwnerReference{
		"opendatahub/maas-api-route": {
			APIVersion: "gateway.networking.k8s.io/v1",
			Kind:       "HTTPRoute",
			Name:       "maas-api-route",
			UID:        routeUID,
		},
	}

	setHTTPRouteOwnerRef(u, routes)
	assert.Len(t, u.GetOwnerReferences(), 1)

	setHTTPRouteOwnerRef(u, routes)
	assert.Len(t, u.GetOwnerReferences(), 1, "must not duplicate ownerReference")
}
