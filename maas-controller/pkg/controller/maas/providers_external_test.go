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
	"strings"
	"testing"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func newExternalModel(name, ns, provider, endpoint string) *maasv1alpha1.MaaSModelRef {
	return &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind:     "ExternalModel",
				Name:     name,
				Provider: provider,
				Endpoint: endpoint,
			},
		},
	}
}

func newHTTPRouteWithGateway(name, ns, gatewayName, gatewayNS string) *gatewayapiv1.HTTPRoute {
	gwNS := gatewayapiv1.Namespace(gatewayNS)
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gatewayapiv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{Name: gatewayapiv1.ObjectName(gatewayName), Namespace: &gwNS},
				},
			},
		},
		Status: gatewayapiv1.HTTPRouteStatus{
			RouteStatus: gatewayapiv1.RouteStatus{
				Parents: []gatewayapiv1.RouteParentStatus{
					{
						ParentRef: gatewayapiv1.ParentReference{
							Name:      gatewayapiv1.ObjectName(gatewayName),
							Namespace: &gwNS,
						},
						Conditions: []metav1.Condition{
							{Type: string(gatewayapiv1.RouteConditionAccepted), Status: metav1.ConditionTrue},
							{Type: routeConditionProgrammed, Status: metav1.ConditionTrue},
						},
					},
				},
			},
		},
	}
}

func newGatewayWithHostname(name, ns, hostname string) *gatewayapiv1.Gateway {
	h := gatewayapiv1.Hostname(hostname)
	return &gatewayapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gatewayapiv1.GatewaySpec{
			Listeners: []gatewayapiv1.Listener{
				{Name: "https", Hostname: &h},
			},
		},
	}
}

func TestExternalModel_ReconcileRoute_Success(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	route := newHTTPRouteWithGateway("maas-model-gpt-4o", "default", "maas-default-gateway", "openshift-ingress")

	r, _ := newTestReconciler(model, route)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err != nil {
		t.Fatalf("ReconcileRoute: unexpected error: %v", err)
	}

	if model.Status.HTTPRouteName != "maas-model-gpt-4o" {
		t.Errorf("HTTPRouteName = %q, want %q", model.Status.HTTPRouteName, "maas-model-gpt-4o")
	}
	if model.Status.HTTPRouteGatewayName != "maas-default-gateway" {
		t.Errorf("HTTPRouteGatewayName = %q, want %q", model.Status.HTTPRouteGatewayName, "maas-default-gateway")
	}
}

func TestExternalModel_ReconcileRoute_MissingHTTPRoute(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	// Pre-populate status to verify it gets cleared
	model.Status.HTTPRouteName = "stale-route"
	model.Status.Endpoint = "https://stale.example.com/gpt-4o"

	r, _ := newTestReconciler(model)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err != nil {
		t.Fatalf("ReconcileRoute: expected nil error for missing HTTPRoute (Pending), got: %v", err)
	}
	if model.Status.HTTPRouteName != "" {
		t.Errorf("HTTPRouteName should be cleared, got %q", model.Status.HTTPRouteName)
	}
	if model.Status.Endpoint != "" {
		t.Errorf("Endpoint should be cleared, got %q", model.Status.Endpoint)
	}
}

func TestExternalModel_ReconcileRoute_MissingProvider(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "api.openai.com")

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err == nil {
		t.Fatal("ReconcileRoute: expected error for missing provider")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("ReconcileRoute: error = %q, want to contain 'provider'", err.Error())
	}
}

func TestExternalModel_ReconcileRoute_WrongGateway(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	route := newHTTPRouteWithGateway("maas-model-gpt-4o", "default", "wrong-gateway", "wrong-ns")

	r, _ := newTestReconciler(model, route)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err == nil {
		t.Fatal("ReconcileRoute: expected error for wrong gateway")
	}
	if !strings.Contains(err.Error(), "does not reference gateway") {
		t.Errorf("ReconcileRoute: error = %q, want to contain 'does not reference gateway'", err.Error())
	}
}

func TestExternalModel_Status_Ready(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	model.Status.HTTPRouteName = "maas-model-gpt-4o"
	model.Status.HTTPRouteGatewayName = "maas-default-gateway"
	model.Status.HTTPRouteHostnames = []string{"maas.example.com"}

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	endpoint, ready, err := handler.Status(context.Background(), log, model)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if !ready {
		t.Error("Status: ready = false, want true")
	}
	if endpoint != "https://maas.example.com/gpt-4o" {
		t.Errorf("Status: endpoint = %q, want %q", endpoint, "https://maas.example.com/gpt-4o")
	}
}

func TestExternalModel_Status_NotReadyWhenGatewayNotAccepted(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	// HTTPRouteName set but gateway not yet accepted (no HTTPRouteGatewayName)
	model.Status.HTTPRouteName = "maas-model-gpt-4o"

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	_, ready, err := handler.Status(context.Background(), log, model)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if ready {
		t.Error("Status: ready = true, want false (gateway not yet accepted)")
	}
}

func TestExternalModel_Status_NotReady(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	_, ready, err := handler.Status(context.Background(), log, model)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if ready {
		t.Error("Status: ready = true, want false")
	}
}

func TestExternalModel_GetModelEndpoint_FromHostnames(t *testing.T) {
	model := newExternalModel("claude-sonnet", "default", "anthropic", "api.anthropic.com")
	model.Status.HTTPRouteHostnames = []string{"maas.example.com"}

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	endpoint, err := handler.GetModelEndpoint(context.Background(), log, model)
	if err != nil {
		t.Fatalf("GetModelEndpoint: unexpected error: %v", err)
	}
	if endpoint != "https://maas.example.com/claude-sonnet" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.example.com/claude-sonnet")
	}
}

func TestExternalModel_GetModelEndpoint_FromGateway(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	gateway := newGatewayWithHostname("maas-default-gateway", "openshift-ingress", "maas.cluster.example.com")

	r, _ := newTestReconciler(model, gateway)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	endpoint, err := handler.GetModelEndpoint(context.Background(), log, model)
	if err != nil {
		t.Fatalf("GetModelEndpoint: unexpected error: %v", err)
	}
	if endpoint != "https://maas.cluster.example.com/gpt-4o" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.cluster.example.com/gpt-4o")
	}
}

func TestExternalModel_CleanupOnDelete(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.CleanupOnDelete(context.Background(), log, model)
	if err != nil {
		t.Fatalf("CleanupOnDelete: unexpected error: %v", err)
	}
}

func TestExternalModel_CredentialRef(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	model.Spec.CredentialRef = &maasv1alpha1.CredentialReference{
		Name:      "openai-api-key",
		Namespace: "opendatahub",
	}

	if model.Spec.CredentialRef.Name != "openai-api-key" {
		t.Errorf("CredentialRef.Name = %q, want %q", model.Spec.CredentialRef.Name, "openai-api-key")
	}
	if model.Spec.CredentialRef.Namespace != "opendatahub" {
		t.Errorf("CredentialRef.Namespace = %q, want %q", model.Spec.CredentialRef.Namespace, "opendatahub")
	}
}

func TestExternalModelRouteResolver(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	resolver := externalModelRouteResolver{}

	routeName, routeNS, err := resolver.HTTPRouteForModel(context.Background(), nil, model)
	if err != nil {
		t.Fatalf("HTTPRouteForModel: unexpected error: %v", err)
	}
	if routeName != "maas-model-gpt-4o" {
		t.Errorf("routeName = %q, want %q", routeName, "maas-model-gpt-4o")
	}
	if routeNS != "default" {
		t.Errorf("routeNS = %q, want %q", routeNS, "default")
	}
}
