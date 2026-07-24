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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/modelnaming"
)

func newExternalModel(name, ns, provider, endpoint string) *maasv1alpha1.MaaSModelRef {
	return &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{
				Kind: "ExternalModel",
				Name: name,
			},
		},
	}
}

func newExternalModelCR(name, ns, provider, endpoint string) *maasv1alpha1.ExternalModel {
	return &maasv1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.ExternalModelSpec{
			Provider: provider,
			Endpoint: endpoint,
			CredentialRef: maasv1alpha1.CredentialReference{
				Name: name + "-api-key",
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
	externalModelCR := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	route := newHTTPRouteWithGateway(modelnaming.ExternalModelResourceName("gpt-4o"), "default", "maas-default-gateway", "openshift-ingress")

	r, _ := newTestReconciler(model, externalModelCR, route)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err != nil {
		t.Fatalf("ReconcileRoute: unexpected error: %v", err)
	}

	if model.Status.HTTPRouteName != "maas-gpt-4o" {
		t.Errorf("HTTPRouteName = %q, want %q", model.Status.HTTPRouteName, "maas-gpt-4o")
	}
	if model.Status.HTTPRouteGatewayName != "maas-default-gateway" {
		t.Errorf("HTTPRouteGatewayName = %q, want %q", model.Status.HTTPRouteGatewayName, "maas-default-gateway")
	}
}

func TestExternalModel_ReconcileRoute_MissingHTTPRoute(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	externalModelCR := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	// Pre-populate status to verify it gets cleared
	model.Status.HTTPRouteName = "stale-route"
	model.Status.Endpoint = "https://stale.example.com/gpt-4o"

	r, _ := newTestReconciler(model, externalModelCR)
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

func TestExternalModel_ReconcileRoute_MissingExternalModel(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	// Don't create ExternalModel CR - it should fail

	r, _ := newTestReconciler(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err == nil {
		t.Fatal("ReconcileRoute: expected error for missing ExternalModel CR")
	}
	if !strings.Contains(err.Error(), "ExternalModel") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("ReconcileRoute: error = %q, want to contain 'ExternalModel' and 'not found'", err.Error())
	}
}

func TestExternalModel_ReconcileRoute_WrongGateway(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	externalModelCR := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	route := newHTTPRouteWithGateway(modelnaming.ExternalModelResourceName("gpt-4o"), "default", "wrong-gateway", "wrong-ns")

	r, _ := newTestReconciler(model, externalModelCR, route)
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
	model.Status.HTTPRouteName = "maas-gpt-4o"
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
	if endpoint != "https://maas.example.com/default/gpt-4o" {
		t.Errorf("Status: endpoint = %q, want %q", endpoint, "https://maas.example.com/default/gpt-4o")
	}
}

func TestExternalModel_Status_NotReadyWhenGatewayNotAccepted(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	// HTTPRouteName set but gateway not yet accepted (no HTTPRouteGatewayName)
	model.Status.HTTPRouteName = "gpt-4o"

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
	if endpoint != "https://maas.example.com/default/claude-sonnet" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.example.com/default/claude-sonnet")
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
	if endpoint != "https://maas.cluster.example.com/default/gpt-4o" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.cluster.example.com/default/gpt-4o")
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
	externalModel := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	externalModel.Spec.CredentialRef = maasv1alpha1.CredentialReference{
		Name: "openai-api-key",
	}

	if externalModel.Spec.CredentialRef.Name != "openai-api-key" {
		t.Errorf("CredentialRef.Name = %q, want %q", externalModel.Spec.CredentialRef.Name, "openai-api-key")
	}
}

func newInferenceExternalModelCR(name, ns, providerRef string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(inferenceExternalModelGVK)
	obj.SetName(name)
	obj.SetNamespace(ns)
	obj.Object["spec"] = map[string]any{
		"externalProviderRefs": []any{
			map[string]any{
				"ref":         map[string]any{"name": providerRef},
				"targetModel": "gpt-4o",
				"apiFormat":   "openai-chat",
			},
		},
	}
	obj.Object["status"] = map[string]any{
		"httpRouteName": name,
	}
	return obj
}

func newTestReconcilerWithMapper(objects ...client.Object) (*MaaSModelRefReconciler, client.Client) {
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objects...).
		WithStatusSubresource(&maasv1alpha1.MaaSModelRef{}).
		Build()
	return &MaaSModelRefReconciler{
		Client:           c,
		Scheme:           scheme,
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "openshift-ingress",
	}, c
}

func TestExternalModel_ReconcileRoute_InferenceExternalModel(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "")
	inferenceEM := newInferenceExternalModelCR("gpt-4o", "default", "openai-provider")
	// HTTPRoute name comes from inference ExternalModel status.httpRouteName ("gpt-4o")
	route := newHTTPRouteWithGateway("gpt-4o", "default", "maas-default-gateway", "openshift-ingress")

	r, _ := newTestReconcilerWithMapper(model, inferenceEM, route)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err != nil {
		t.Fatalf("ReconcileRoute: unexpected error: %v", err)
	}

	if model.Status.HTTPRouteName != "gpt-4o" {
		t.Errorf("HTTPRouteName = %q, want %q", model.Status.HTTPRouteName, "gpt-4o")
	}
	if model.Status.HTTPRouteGatewayName != "maas-default-gateway" {
		t.Errorf("HTTPRouteGatewayName = %q, want %q", model.Status.HTTPRouteGatewayName, "maas-default-gateway")
	}
}

func TestExternalModel_ReconcileRoute_BothMissing(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "")

	r, _ := newTestReconcilerWithMapper(model)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err == nil {
		t.Fatal("ReconcileRoute: expected error when both ExternalModel types are missing")
	}
	if !strings.Contains(err.Error(), "maas.opendatahub.io") || !strings.Contains(err.Error(), "inference.opendatahub.io") {
		t.Errorf("error should mention both API groups, got: %v", err)
	}
}

func TestExternalModel_ReconcileRoute_InferencePreferredOverLegacy(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "")
	maasEM := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	inferenceEM := newInferenceExternalModelCR("gpt-4o", "default", "different-provider")
	// Route name from inference ExternalModel status.httpRouteName
	route := newHTTPRouteWithGateway("gpt-4o", "default", "maas-default-gateway", "openshift-ingress")

	r, _ := newTestReconcilerWithMapper(model, maasEM, inferenceEM, route)
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	err := handler.ReconcileRoute(context.Background(), log, model)
	if err != nil {
		t.Fatalf("ReconcileRoute: unexpected error: %v", err)
	}

	if model.Status.HTTPRouteGatewayName != "maas-default-gateway" {
		t.Errorf("HTTPRouteGatewayName = %q, want %q", model.Status.HTTPRouteGatewayName, "maas-default-gateway")
	}
}

func TestExternalModelRouteResolver(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	resolver := externalModelRouteResolver{}

	routeName, routeNS, err := resolver.HTTPRouteForModel(context.Background(), nil, model)
	if err != nil {
		t.Fatalf("HTTPRouteForModel: unexpected error: %v", err)
	}
	if routeName != "maas-gpt-4o" {
		t.Errorf("routeName = %q, want %q", routeName, "maas-gpt-4o")
	}
	if routeNS != "default" {
		t.Errorf("routeNS = %q, want %q", routeNS, "default")
	}
}

func TestExternalModelRouteResolver_FromInferenceStatus(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "")
	inferenceEM := newInferenceExternalModelCR("gpt-4o", "default", "openai-provider")

	_, c := newTestReconcilerWithMapper(model, inferenceEM)
	resolver := externalModelRouteResolver{}

	routeName, routeNS, err := resolver.HTTPRouteForModel(context.Background(), c, model)
	if err != nil {
		t.Fatalf("HTTPRouteForModel: unexpected error: %v", err)
	}
	if routeName != "gpt-4o" {
		t.Errorf("routeName = %q, want %q (from inference status)", routeName, "gpt-4o")
	}
	if routeNS != "default" {
		t.Errorf("routeNS = %q, want %q", routeNS, "default")
	}
}

func TestExternalModelRouteResolver_InferenceStatusEmpty_ReturnsNotReady(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "", "")

	// Inference ExternalModel exists but status.httpRouteName is not set yet
	inferenceEM := newInferenceExternalModelCR("gpt-4o", "default", "openai-provider")
	inferenceEM.Object["status"] = map[string]any{}

	_, c := newTestReconcilerWithMapper(model, inferenceEM)
	resolver := externalModelRouteResolver{}

	_, _, err := resolver.HTTPRouteForModel(context.Background(), c, model)
	if err == nil {
		t.Fatal("HTTPRouteForModel: expected ErrHTTPRouteNotFound when status.httpRouteName is empty, got nil")
	}
	if !errors.Is(err, ErrHTTPRouteNotFound) {
		t.Errorf("HTTPRouteForModel: want ErrHTTPRouteNotFound, got %v", err)
	}
}
