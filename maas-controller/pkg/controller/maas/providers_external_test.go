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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
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

func TestExternalModel_ReconcileRoute_GatewayStopsAccepting(t *testing.T) {
	model := newExternalModel("gpt-4o", "default", "openai", "api.openai.com")
	externalModelCR := newExternalModelCR("gpt-4o", "default", "openai", "api.openai.com")
	route := newHTTPRouteWithGateway(modelnaming.ExternalModelResourceName("gpt-4o"), "default", "maas-default-gateway", "openshift-ingress")
	route.Status.Parents[0].Conditions[0].Status = metav1.ConditionFalse
	route.Status.Parents[0].Conditions[0].Reason = string(gatewayapiv1.RouteReasonNotAllowedByListeners)
	// Status left by an earlier reconcile, when the gateway still accepted the route.
	model.Status.HTTPRouteName = route.Name
	model.Status.HTTPRouteNamespace = route.Namespace
	model.Status.HTTPRouteGatewayName = "maas-default-gateway"
	model.Status.HTTPRouteGatewayNamespace = "openshift-ingress"
	model.Status.HTTPRouteHostnames = []string{"api.example.com"}

	r, _ := newTestReconciler(model, externalModelCR, route)
	r.GatewayName = "maas-default-gateway"
	r.GatewayNamespace = "openshift-ingress"
	handler := &externalModelHandler{r: r}
	log := zap.New(zap.UseDevMode(true))

	if err := handler.ReconcileRoute(t.Context(), log, model); err != nil {
		t.Fatalf("ReconcileRoute: unexpected error: %v", err)
	}

	if model.Status.HTTPRouteGatewayName != "" || model.Status.HTTPRouteGatewayNamespace != "" || model.Status.HTTPRouteHostnames != nil {
		t.Errorf("gateway status kept after the gateway stopped accepting the route: gateway=%s/%s hostnames=%v",
			model.Status.HTTPRouteGatewayNamespace, model.Status.HTTPRouteGatewayName, model.Status.HTTPRouteHostnames)
	}
	_, ready, err := handler.Status(t.Context(), log, model)
	if err != nil {
		t.Fatalf("Status: unexpected error: %v", err)
	}
	if ready {
		t.Error("model reported ready although its gateway no longer accepts the route")
	}
	if reason, _ := handler.NotReadyReason(); reason != maasv1alpha1.ReasonNotAccepted {
		t.Errorf("NotReadyReason = %q, want %q", reason, maasv1alpha1.ReasonNotAccepted)
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
	if !strings.Contains(err.Error(), "no AITenant found") {
		t.Errorf("ReconcileRoute: error = %q, want to contain 'no AITenant found'", err.Error())
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
	if endpoint != "https://maas.example.com" {
		t.Errorf("Status: endpoint = %q, want %q", endpoint, "https://maas.example.com")
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

// The HTTPRoute watch filters route status down to accepted parents, so a gateway
// accepting the route must still reach the reconcile and make the model Ready.
func TestExternalModel_ReadyFollowsRouteAccepted(t *testing.T) {
	ctx := t.Context()
	const (
		modelName = "gpt-4o"
		ns        = "default"
	)
	model := newExternalModel(modelName, ns, "openai", "api.openai.com")
	route := newHTTPRouteWithGateway(modelnaming.ExternalModelResourceName(modelName), ns, testGatewayName, testGatewayNamespace)
	route.Spec.Hostnames = []gatewayapiv1.Hostname{"maas.example.com"}
	apimeta.SetStatusCondition(&route.Status.Parents[0].Conditions, metav1.Condition{
		Type: string(gatewayapiv1.RouteConditionAccepted), Status: metav1.ConditionUnknown, Reason: string(gatewayapiv1.RouteReasonPending),
	})
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", modelName, 100)
	sub.Spec.ModelRefs[0].Namespace = ns
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: ns})
	r, c := newTestReconciler(model, newExternalModelCR(modelName, ns, "openai", "api.openai.com"), route, sub, auth)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (route not accepted): %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != "Unhealthy" {
		t.Errorf("Phase before route accepted = %q, want Unhealthy", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, maasv1alpha1.ConditionRuntimeReady, metav1.ConditionFalse, string(maasv1alpha1.ReasonRuntimeHealthFailure))

	current := &gatewayapiv1.HTTPRoute{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
		t.Fatalf("Get HTTPRoute: %v", err)
	}
	accepted := current.DeepCopy()
	apimeta.SetStatusCondition(&accepted.Status.Parents[0].Conditions, metav1.Condition{
		Type: string(gatewayapiv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayapiv1.RouteReasonAccepted),
	})
	if !httpRouteChangedForModelRef().Update(event.UpdateEvent{ObjectOld: current, ObjectNew: accepted}) {
		t.Fatal("HTTPRoute watch predicate dropped the Accepted transition")
	}
	if err := c.Status().Update(ctx, accepted); err != nil {
		t.Fatalf("Update HTTPRoute status: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (route accepted): %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Errorf("Phase after route accepted = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Endpoint != "https://maas.example.com" {
		t.Errorf("Endpoint = %q, want %q", got.Status.Endpoint, "https://maas.example.com")
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
	if endpoint != "https://maas.example.com" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.example.com")
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
	if endpoint != "https://maas.cluster.example.com" {
		t.Errorf("GetModelEndpoint = %q, want %q", endpoint, "https://maas.cluster.example.com")
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

// newTestReconcilerWithMapper is newTestReconciler with a REST mapper that knows the
// inference ExternalModel, so its reads succeed instead of falling back to the legacy kind.
func newTestReconcilerWithMapper(objects ...client.Object) (*MaaSModelRefReconciler, client.Client) {
	// Include default AITenant for tenant auto-resolution
	allObjects := append([]client.Object{defaultTestAITenant()}, objects...)
	inferenceEM := &unstructured.Unstructured{}
	inferenceEM.SetGroupVersionKind(inferenceExternalModelGVK)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(allObjects...).
		WithStatusSubresource(&maasv1alpha1.MaaSModelRef{}, &gatewayapiv1.HTTPRoute{}, inferenceEM).
		WithIndex(&maasv1alpha1.MaaSModelRef{}, modelRefNameIndex, modelRefNameIndexer).
		WithIndex(&maasv1alpha1.MaaSModelRef{}, tenantAssociationIndex, tenantAssociationIndexer).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()
	return &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
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

// The inference ExternalModel reconciler can record status.httpRouteName after the
// gateway has accepted the route, leaving the HTTPRoute watch nothing to deliver, so the
// ExternalModel watch has to bring the model Ready.
func TestExternalModel_ReadyFollowsInferenceRouteName(t *testing.T) {
	ctx := t.Context()
	const (
		modelName = "gpt-4o"
		ns        = "default"
	)
	model := newExternalModel(modelName, ns, "", "")
	inferenceEM := newInferenceExternalModelCR(modelName, ns, "openai-provider")
	delete(inferenceEM.Object, "status")
	route := newHTTPRouteWithGateway(modelName, ns, testGatewayName, testGatewayNamespace)
	route.Spec.Hostnames = []gatewayapiv1.Hostname{"maas.example.com"}
	sub := newMaaSSubscription("sub1", "admin-ns", "team-a", modelName, 100)
	sub.Spec.ModelRefs[0].Namespace = ns
	auth := newMaaSAuthPolicy("auth1", "admin-ns", "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: ns})
	r, c := newTestReconcilerWithMapper(model, inferenceEM, route, sub, auth)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: ns}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (route name not recorded): %v", err)
	}
	got := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase == "Ready" || got.Status.Endpoint != "" {
		t.Fatalf("before status.httpRouteName: Phase = %q, Endpoint = %q, want not Ready with no endpoint", got.Status.Phase, got.Status.Endpoint)
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(inferenceExternalModelGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(inferenceEM), current); err != nil {
		t.Fatalf("Get inference ExternalModel: %v", err)
	}
	recorded := current.DeepCopy()
	if err := unstructured.SetNestedField(recorded.Object, modelName, "status", "httpRouteName"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	if !ippExternalModelChangedForModelRef().Update(event.UpdateEvent{ObjectOld: current, ObjectNew: recorded}) {
		t.Fatal("ExternalModel watch predicate dropped the status.httpRouteName write")
	}
	if reqs := r.mapIPPExternalModelToMaaSModelRefs(ctx, recorded); len(reqs) != 1 || reqs[0] != req {
		t.Fatalf("mapIPPExternalModelToMaaSModelRefs = %v, want [%v]", reqs, req)
	}
	if err := c.Status().Update(ctx, recorded); err != nil {
		t.Fatalf("Update inference ExternalModel status: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (route name recorded): %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Errorf("Phase after status.httpRouteName = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Endpoint != "https://maas.example.com" {
		t.Errorf("Endpoint = %q, want %q", got.Status.Endpoint, "https://maas.example.com")
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
