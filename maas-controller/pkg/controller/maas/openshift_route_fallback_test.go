/*
Copyright 2025.

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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestLLMISvcGetModelEndpointFallsBackToOpenShiftRoute(t *testing.T) {
	model := newMaaSModelRefForLLMISvc("test-model", "default", "test-llmisvc")
	model.Status.HTTPRouteGatewayName = "tenant-gateway"
	model.Status.HTTPRouteGatewayNamespace = "tenant-ingress"

	r, _ := newTestReconcilerWithMapper(
		model,
		newGatewayWithStatusAddress("tenant-gateway", "tenant-ingress", "tenant-gateway.tenant-ingress.svc.cluster.local"),
		newGatewayService("tenant-gateway-service", "tenant-ingress", "tenant-gateway"),
		newAdmittedRoute("tenant-gateway-route-http", "tenant-ingress", "z-maas.apps.example.com", "tenant-gateway-service", false),
		newAdmittedRoute("tenant-gateway-route-https", "tenant-ingress", "a-maas.apps.example.com", "tenant-gateway-service", true),
	)
	handler := &llmisvcHandler{r: r}

	endpoint, err := handler.GetModelEndpoint(context.Background(), zap.New(zap.UseDevMode(true)), model)
	if err != nil {
		t.Fatalf("GetModelEndpoint: unexpected error: %v", err)
	}
	if endpoint != "https://a-maas.apps.example.com/test-model" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://a-maas.apps.example.com/test-model")
	}
}

func TestLLMISvcStatusFallsBackToOpenShiftRoute(t *testing.T) {
	model := newMaaSModelRefForLLMISvc("test-model", "default", "test-llmisvc")
	model.Status.HTTPRouteGatewayName = "tenant-gateway"
	model.Status.HTTPRouteGatewayNamespace = "tenant-ingress"
	llmisvc := newReadyLLMISvc("test-llmisvc", "default", []duckv1.Addressable{{
		Name: strPtr("gateway-external"),
		URL:  mustParseURL("http://tenant-gateway.tenant-ingress.svc.cluster.local/test-model"),
	}})
	llmisvc.Status.Conditions = duckv1.Conditions{{Type: "Ready", Status: "True"}}

	r, _ := newTestReconcilerWithMapper(
		model,
		llmisvc,
		newGatewayWithStatusAddress("tenant-gateway", "tenant-ingress", "tenant-gateway.tenant-ingress.svc.cluster.local"),
		newGatewayService("tenant-gateway-service", "tenant-ingress", "tenant-gateway"),
		newAdmittedRoute("tenant-gateway-route", "tenant-ingress", "maas.apps.example.com", "tenant-gateway-service", true),
	)
	handler := &llmisvcHandler{r: r}

	endpoint, ready, err := handler.Status(context.Background(), zap.New(zap.UseDevMode(true)), model)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if !ready {
		t.Fatal("Status: ready = false, want true")
	}
	if endpoint != "https://maas.apps.example.com/test-model" {
		t.Errorf("Status endpoint = %q, want %q", endpoint, "https://maas.apps.example.com/test-model")
	}
}

func TestExternalModelGetModelEndpointFallsBackToOpenShiftRoute(t *testing.T) {
	model := newExternalModel("shared", "default", "", "")
	model.Status.HTTPRouteGatewayName = "tenant-gateway"
	model.Status.HTTPRouteGatewayNamespace = "tenant-ingress"

	r, _ := newTestReconcilerWithMapper(
		model,
		newGatewayWithStatusAddress("tenant-gateway", "tenant-ingress", "tenant-gateway.tenant-ingress.svc.cluster.local"),
		newGatewayService("tenant-gateway-service", "tenant-ingress", "tenant-gateway"),
		newAdmittedRoute("tenant-gateway-route", "tenant-ingress", "maas.apps.example.com", "tenant-gateway-service", true),
	)
	handler := &externalModelHandler{r: r}

	endpoint, err := handler.GetModelEndpoint(context.Background(), zap.New(zap.UseDevMode(true)), model)
	if err != nil {
		t.Fatalf("GetModelEndpoint: unexpected error: %v", err)
	}
	if endpoint != "https://maas.apps.example.com" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.apps.example.com")
	}
}

func TestExternalModelGetModelEndpointKeepsHTTPRouteHostname(t *testing.T) {
	model := newExternalModel("shared", "default", "", "")
	model.Status.HTTPRouteGatewayName = "tenant-gateway"
	model.Status.HTTPRouteGatewayNamespace = "tenant-ingress"
	model.Status.HTTPRouteHostnames = []string{"maas.example.com"}

	r, _ := newTestReconcilerWithMapper(
		model,
		newGatewayWithStatusAddress("tenant-gateway", "tenant-ingress", "tenant-gateway.tenant-ingress.svc.cluster.local"),
		newGatewayService("tenant-gateway-service", "tenant-ingress", "tenant-gateway"),
		newAdmittedRoute("tenant-gateway-route", "tenant-ingress", "other.apps.example.com", "tenant-gateway-service", true),
	)
	handler := &externalModelHandler{r: r}

	endpoint, err := handler.GetModelEndpoint(context.Background(), zap.New(zap.UseDevMode(true)), model)
	if err != nil {
		t.Fatalf("GetModelEndpoint: unexpected error: %v", err)
	}
	if endpoint != "https://maas.example.com" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.example.com")
	}
}

func newGatewayWithStatusAddress(name, namespace, address string) *gatewayapiv1.Gateway {
	addressType := gatewayapiv1.HostnameAddressType
	return &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: gatewayapiv1.GatewayStatus{
			Addresses: []gatewayapiv1.GatewayStatusAddress{
				{Type: &addressType, Value: address},
			},
		},
	}
}

func newGatewayService(name, namespace, gatewayName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{gatewayNameLabel: gatewayName},
		},
	}
}

func newAdmittedRoute(name, namespace, host, serviceName string, tlsEnabled bool) *unstructured.Unstructured {
	spec := map[string]any{
		"to": map[string]any{
			"kind": "Service",
			"name": serviceName,
		},
	}
	if tlsEnabled {
		spec["tls"] = map[string]any{"termination": "reencrypt"}
	}
	route := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "route.openshift.io/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
		"status": map[string]any{
			"ingress": []any{
				map[string]any{
					"host": host,
					"conditions": []any{
						map[string]any{"type": "Admitted", "status": "True"},
					},
				},
			},
		},
	}}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "Route",
	})
	return route
}
