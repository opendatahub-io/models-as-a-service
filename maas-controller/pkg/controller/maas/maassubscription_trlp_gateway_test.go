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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func newDiscoveredTenantNamespace(name, tenant string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: tenant},
		},
	}
}

func newAITenantManagedTenantConfig(namespace, tenantName string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: namespace,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantName,
				tenantreconcile.LabelTenantNamespace:   namespace,
			},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      tenantName,
				tenantreconcile.AnnotationAITenantNamespace: testAITenantNamespace,
			},
		},
	}
}

func newTenantSubscription(name, namespace, modelNamespace, modelName string, limit int64) *maasv1alpha1.MaaSSubscription {
	return &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner: maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team"}}},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{
				Name:            modelName,
				Namespace:       modelNamespace,
				TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: limit, Window: "1m"}},
			}},
		},
	}
}

func gatewayResultWithRoute(gateway types.NamespacedName, routeName string) any {
	return map[string]any{
		"name":         gateway.Name,
		"namespace":    gateway.Namespace,
		"httpRouteRef": map[string]any{"name": routeName, "namespace": "user-ns"},
		"conditions": []any{map[string]any{
			"type": "Ready", "status": string(metav1.ConditionTrue), "reason": "Reconciled", "message": "",
			"lastTransitionTime": "2026-10-12T00:00:00Z",
		}},
	}
}

func getPolicyPredicateIDs(t *testing.T, trlp *unstructured.Unstructured) []string {
	t.Helper()
	limits, found, err := unstructured.NestedMap(trlp.Object, "spec", "limits")
	if err != nil || !found {
		t.Fatalf("spec.limits missing: found=%v err=%v", found, err)
	}
	ids := make([]string, 0)
	for _, rawLimit := range limits {
		limit, ok := rawLimit.(map[string]any)
		if !ok {
			continue
		}
		when, _, _ := unstructured.NestedSlice(limit, "when")
		for _, w := range when {
			m, ok := w.(map[string]any)
			if !ok {
				continue
			}
			pred, _ := m["predicate"].(string)
			for piece := range strings.SplitSeq(pred, "\"") {
				if len(piece) == 16 {
					ids = append(ids, piece)
				}
			}
		}
	}
	return ids
}

func TestReconcileTRLPForModel_SharedExternalModelTargetsTenantGatewayRoutes(t *testing.T) {
	const (
		modelNS = "user-ns"
		subANS  = "tenant-a-admin"
		subBNS  = "tenant-b-admin"
	)

	modelA := newMaaSModelRef("model-a", modelNS, "ExternalModel", "shared")
	modelA.Status.HTTPRouteGatewayName = testGatewayA.Name
	modelA.Status.HTTPRouteGatewayNamespace = testGatewayA.Namespace

	modelB := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	modelB.Status.HTTPRouteGatewayName = testGatewayB.Name
	modelB.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace

	subA := newTenantSubscription("sub-a", subANS, modelNS, "model-a", 100)
	subB := newTenantSubscription("sub-b", subBNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	),
		modelA,
		modelB,
		subA,
		subB,
		newDiscoveredTenantNamespace(subANS, "tenant-a"),
		newDiscoveredTenantNamespace(subBNS, "tenant-b"),
		newAITenantManagedTenantConfig(subANS, "tenant-a"),
		newAITenantManagedTenantConfig(subBNS, "tenant-b"),
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		APIReader:                       c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	for _, req := range []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: subA.Name, Namespace: subA.Namespace}},
		{NamespacedName: types.NamespacedName{Name: subB.Name, Namespace: subB.Namespace}},
	} {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile %s/%s: %v", req.Namespace, req.Name, err)
		}
	}

	tests := []struct {
		modelName     string
		routeName     string
		gateway       types.NamespacedName
		subNS         string
		subName       string
		otherSubNS    string
		otherSubName  string
		otherModelRef string
	}{
		{modelName: "model-a", routeName: "route-gw-a", gateway: testGatewayA, subNS: subANS, subName: "sub-a", otherSubNS: subBNS, otherSubName: "sub-b", otherModelRef: "model-b"},
		{modelName: "model-b", routeName: "route-gw-b", gateway: testGatewayB, subNS: subBNS, subName: "sub-b", otherSubNS: subANS, otherSubName: "sub-a", otherModelRef: "model-a"},
	}
	for _, tt := range tests {
		t.Run(tt.modelName, func(t *testing.T) {
			trlp := &unstructured.Unstructured{}
			trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
			if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-" + tt.modelName, Namespace: modelNS}, trlp); err != nil {
				t.Fatalf("Get TokenRateLimitPolicy: %v", err)
			}

			routeName, _, _ := unstructured.NestedString(trlp.Object, "spec", "targetRef", "name")
			if routeName != tt.routeName {
				t.Fatalf("targetRef.name = %q, want %q", routeName, tt.routeName)
			}

			labels := trlp.GetLabels()
			if got := labels[trlpLabelGatewayName]; got != tt.gateway.Name {
				t.Fatalf("label %s = %q, want %q", trlpLabelGatewayName, got, tt.gateway.Name)
			}
			if got := labels[trlpLabelGatewayNamespace]; got != tt.gateway.Namespace {
				t.Fatalf("label %s = %q, want %q", trlpLabelGatewayNamespace, got, tt.gateway.Namespace)
			}

			expectedID := SubscriptionRateLimitID(ModelScopedSubscriptionKey(tt.subNS, tt.subName, modelNS, tt.modelName))
			otherID := SubscriptionRateLimitID(ModelScopedSubscriptionKey(tt.otherSubNS, tt.otherSubName, modelNS, tt.otherModelRef))
			ids := getPolicyPredicateIDs(t, trlp)
			if !strings.Contains(strings.Join(ids, ","), expectedID) {
				t.Fatalf("predicate IDs %v do not include expected subscription ID %s", ids, expectedID)
			}
			if strings.Contains(strings.Join(ids, ","), otherID) {
				t.Fatalf("predicate IDs %v unexpectedly include other tenant/model ID %s", ids, otherID)
			}
		})
	}
}

func TestCheckTokenRateLimitHealth_IncludesRouteGatewayContext(t *testing.T) {
	const (
		modelNS = "user-ns"
		subNS   = "tenant-b-admin"
	)

	model := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	model.Status.HTTPRouteGatewayName = testGatewayB.Name
	model.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace

	sub := newTenantSubscription("sub-b", subNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	), model)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, APIReader: c, Scheme: scheme}

	statuses := r.checkTokenRateLimitHealth(context.Background(), sub)
	if len(statuses) != 1 {
		t.Fatalf("checkTokenRateLimitHealth returned %d statuses, want 1", len(statuses))
	}
	if statuses[0].Reason != maasv1alpha1.ReasonNotFound {
		t.Fatalf("Reason = %q, want %q", statuses[0].Reason, maasv1alpha1.ReasonNotFound)
	}
	wantGateway := fmt.Sprintf("gateway %s/%s", testGatewayB.Namespace, testGatewayB.Name)
	if !strings.Contains(statuses[0].Message, wantGateway) {
		t.Fatalf("Message %q does not include %q", statuses[0].Message, wantGateway)
	}

	trlp := newPreexistingTRLP("maas-trlp-model-b", modelNS, "model-b", map[string]string{})
	if err := unstructured.SetNestedSlice(trlp.Object, []any{map[string]any{
		"type": "Accepted", "status": "False", "message": "policy conflict",
	}}, "status", "conditions"); err != nil {
		t.Fatalf("SetNestedSlice: %v", err)
	}
	if err := c.Create(context.Background(), trlp); err != nil {
		t.Fatalf("Create TokenRateLimitPolicy: %v", err)
	}

	statuses = r.checkTokenRateLimitHealth(context.Background(), sub)
	if len(statuses) != 1 {
		t.Fatalf("checkTokenRateLimitHealth returned %d statuses, want 1", len(statuses))
	}
	if statuses[0].Reason != maasv1alpha1.ReasonNotAccepted {
		t.Fatalf("Reason = %q, want %q", statuses[0].Reason, maasv1alpha1.ReasonNotAccepted)
	}
	if !strings.Contains(statuses[0].Message, "policy conflict") || !strings.Contains(statuses[0].Message, wantGateway) {
		t.Fatalf("Message = %q, want conflict text and %q", statuses[0].Message, wantGateway)
	}
}

func TestReconcileTRLPForModel_RouteTransitionRetargetsPolicy(t *testing.T) {
	const (
		modelNS = "user-ns"
		subNS   = "tenant-b-admin"
	)

	model := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	model.Status.HTTPRouteGatewayName = testGatewayB.Name
	model.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace
	sub := newTenantSubscription("sub-b", subNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	),
		model,
		sub,
		newDiscoveredTenantNamespace(subNS, "tenant-b"),
		newAITenantManagedTenantConfig(subNS, "tenant-b"),
		newHTTPRouteWithGateway("route-gw-b2", modelNS, testGatewayB.Name, testGatewayB.Namespace),
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		APIReader:                       c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sub.Name, Namespace: sub.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile (route-gw-b): %v", err)
	}

	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy after first reconcile: %v", err)
	}
	if got, _, _ := unstructured.NestedString(trlp.Object, "spec", "targetRef", "name"); got != "route-gw-b" {
		t.Fatalf("first targetRef.name = %q, want %q", got, "route-gw-b")
	}

	currentEM := &unstructured.Unstructured{}
	currentEM.SetGroupVersionKind(inferenceExternalModelGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "shared", Namespace: modelNS}, currentEM); err != nil {
		t.Fatalf("Get inference ExternalModel: %v", err)
	}
	updatedEM := currentEM.DeepCopy()
	updatedEM.Object["status"] = map[string]any{"gateways": []any{
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		gatewayResultWithRoute(testGatewayB, "route-gw-b2"),
	}}
	if err := c.Update(context.Background(), updatedEM); err != nil {
		t.Fatalf("Update inference ExternalModel: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile (route-gw-b2): %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy after retarget: %v", err)
	}
	if got, _, _ := unstructured.NestedString(trlp.Object, "spec", "targetRef", "name"); got != "route-gw-b2" {
		t.Fatalf("retargeted targetRef.name = %q, want %q", got, "route-gw-b2")
	}
	if got := trlp.GetLabels()[trlpLabelGatewayName]; got != testGatewayB.Name {
		t.Fatalf("gateway label name = %q, want %q", got, testGatewayB.Name)
	}
	if got := trlp.GetLabels()[trlpLabelGatewayNamespace]; got != testGatewayB.Namespace {
		t.Fatalf("gateway label namespace = %q, want %q", got, testGatewayB.Namespace)
	}
}

func TestReconcileTRLPForModel_GatewayLabelsFollowSelectedGatewayNotParentRefOrder(t *testing.T) {
	const (
		modelNS = "user-ns"
		subNS   = "tenant-b-admin"
	)

	model := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	model.Status.HTTPRouteGatewayName = testGatewayB.Name
	model.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace
	sub := newTenantSubscription("sub-b", subNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	),
		model,
		sub,
		newDiscoveredTenantNamespace(subNS, "tenant-b"),
		newAITenantManagedTenantConfig(subNS, "tenant-b"),
	)

	for _, obj := range objs {
		route, ok := obj.(*gatewayapiv1.HTTPRoute)
		if !ok || route.Name != "route-gw-b" || route.Namespace != modelNS {
			continue
		}
		gwANS := gatewayapiv1.Namespace(testGatewayA.Namespace)
		route.Spec.ParentRefs = append([]gatewayapiv1.ParentReference{{
			Name:      gatewayapiv1.ObjectName(testGatewayA.Name),
			Namespace: &gwANS,
		}}, route.Spec.ParentRefs...)
		break
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		APIReader:                       c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sub.Name, Namespace: sub.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}

	if got := trlp.GetLabels()[trlpLabelGatewayName]; got != testGatewayB.Name {
		t.Fatalf("gateway label name = %q, want selected gateway %q", got, testGatewayB.Name)
	}
	if got := trlp.GetLabels()[trlpLabelGatewayNamespace]; got != testGatewayB.Namespace {
		t.Fatalf("gateway label namespace = %q, want selected gateway namespace %q", got, testGatewayB.Namespace)
	}
}

func TestReconcileTRLPForModel_OwnerOnlyChangeIsPersisted(t *testing.T) {
	const (
		modelNS = "user-ns"
		subNS   = "tenant-b-admin"
	)

	model := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	model.Status.HTTPRouteGatewayName = testGatewayB.Name
	model.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace
	sub := newTenantSubscription("sub-b", subNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	),
		model,
		sub,
		newDiscoveredTenantNamespace(subNS, "tenant-b"),
		newAITenantManagedTenantConfig(subNS, "tenant-b"),
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		APIReader:                       c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sub.Name, Namespace: sub.Namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}

	route := &gatewayapiv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "route-gw-b", Namespace: modelNS}, route); err != nil {
		t.Fatalf("Get route-gw-b: %v", err)
	}

	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}

	// Force an owner-only drift: same owner name/kind, stale UID.
	trlp.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: gatewayapiv1.GroupVersion.String(),
		Kind:       "HTTPRoute",
		Name:       route.Name,
		UID:        types.UID("00000000-0000-0000-0000-000000000000"),
		Controller: ptrTo(true),
	}})
	if err := c.Update(context.Background(), trlp); err != nil {
		t.Fatalf("Update TokenRateLimitPolicy with stale owner UID: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile (owner correction): %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy after owner correction: %v", err)
	}
	if len(trlp.GetOwnerReferences()) != 1 {
		t.Fatalf("ownerRefs len = %d, want 1", len(trlp.GetOwnerReferences()))
	}
	if got := trlp.GetOwnerReferences()[0].UID; got != route.UID {
		t.Fatalf("ownerRef UID = %q, want %q", got, route.UID)
	}
}

func TestReconcileTRLPForModel_RemovedTenantReferenceCleansOnlyItsPolicy(t *testing.T) {
	const (
		modelNS = "user-ns"
		subANS  = "tenant-a-admin"
		subBNS  = "tenant-b-admin"
	)

	modelA := newMaaSModelRef("model-a", modelNS, "ExternalModel", "shared")
	modelA.Status.HTTPRouteGatewayName = testGatewayA.Name
	modelA.Status.HTTPRouteGatewayNamespace = testGatewayA.Namespace

	modelB := newMaaSModelRef("model-b", modelNS, "ExternalModel", "shared")
	modelB.Status.HTTPRouteGatewayName = testGatewayB.Name
	modelB.Status.HTTPRouteGatewayNamespace = testGatewayB.Namespace

	subA := newTenantSubscription("sub-a", subANS, modelNS, "model-a", 100)
	subB := newTenantSubscription("sub-b", subBNS, modelNS, "model-b", 200)

	objs := append(newSharedExternalModelObjects(
		newGatewayResult(testGatewayA, metav1.ConditionTrue),
		newGatewayResult(testGatewayB, metav1.ConditionTrue),
	),
		modelA,
		modelB,
		subA,
		subB,
		newDiscoveredTenantNamespace(subANS, "tenant-a"),
		newDiscoveredTenantNamespace(subBNS, "tenant-b"),
		newAITenantManagedTenantConfig(subANS, "tenant-a"),
		newAITenantManagedTenantConfig(subBNS, "tenant-b"),
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		APIReader:                       c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	for _, req := range []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: subA.Name, Namespace: subA.Namespace}},
		{NamespacedName: types.NamespacedName{Name: subB.Name, Namespace: subB.Namespace}},
	} {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile %s/%s: %v", req.Namespace, req.Name, err)
		}
	}

	trlpA := &unstructured.Unstructured{}
	trlpA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-a", Namespace: modelNS}, trlpA); err != nil {
		t.Fatalf("Get TRLP model-a before removal: %v", err)
	}
	targetA, _, _ := unstructured.NestedString(trlpA.Object, "spec", "targetRef", "name")
	labelsA := trlpA.GetLabels()

	trlpB := &unstructured.Unstructured{}
	trlpB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlpB); err != nil {
		t.Fatalf("Get TRLP model-b before removal: %v", err)
	}

	// Remove model-b reference from tenant-b subscription.
	if err := c.Get(context.Background(), types.NamespacedName{Name: subB.Name, Namespace: subB.Namespace}, subB); err != nil {
		t.Fatalf("Reload sub-b: %v", err)
	}
	subB.Spec.ModelRefs = nil
	if err := c.Update(context.Background(), subB); err != nil {
		t.Fatalf("Update sub-b to remove modelRefs: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: subB.Name, Namespace: subB.Namespace}}); err != nil {
		t.Fatalf("Reconcile sub-b after modelRef removal: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-b", Namespace: modelNS}, trlpB); !apierrors.IsNotFound(err) {
		t.Fatalf("expected TRLP model-b to be removed, got err=%v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "maas-trlp-model-a", Namespace: modelNS}, trlpA); err != nil {
		t.Fatalf("TRLP model-a should remain: %v", err)
	}
	if got, _, _ := unstructured.NestedString(trlpA.Object, "spec", "targetRef", "name"); got != targetA {
		t.Fatalf("TRLP model-a target changed: got %q want %q", got, targetA)
	}
	if got := trlpA.GetLabels()[trlpLabelGatewayName]; got != labelsA[trlpLabelGatewayName] {
		t.Fatalf("TRLP model-a gateway-name changed: got %q want %q", got, labelsA[trlpLabelGatewayName])
	}
	if got := trlpA.GetLabels()[trlpLabelGatewayNamespace]; got != labelsA[trlpLabelGatewayNamespace] {
		t.Fatalf("TRLP model-a gateway-namespace changed: got %q want %q", got, labelsA[trlpLabelGatewayNamespace])
	}
}

func ptrTo[T any](v T) *T { return &v }
