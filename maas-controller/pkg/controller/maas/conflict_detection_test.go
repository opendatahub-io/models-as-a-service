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
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// newRogueAuthPolicy creates a non-MaaS AuthPolicy targeting a specific HTTPRoute.
func newRogueAuthPolicy(name, namespace, httpRouteName string) *unstructured.Unstructured {
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	ap.SetName(name)
	ap.SetNamespace(namespace)
	ap.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": "kserve-controller",
	})
	_ = unstructured.SetNestedMap(ap.Object, map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "HTTPRoute",
			"name":  httpRouteName,
		},
	}, "spec")
	return ap
}

// TestDetectConflictingAuthPolicies_NoConflicts verifies that when only
// MaaS-managed AuthPolicies exist on the HTTPRoute, no conflicts are detected.
func TestDetectConflictingAuthPolicies_NoConflicts(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		authPolicyName = "maas-auth-" + modelName
		maasPolicyName = "policy-a"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	maasAP := newPreexistingAuthPolicy(authPolicyName, namespace, modelName, nil)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, maasAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False, got %v", cond.Status)
	}
	if cond.Reason != "NoConflict" {
		t.Errorf("expected reason NoConflict, got %q", cond.Reason)
	}
}

// TestDetectConflictingAuthPolicies_RogueDetected verifies that a non-MaaS
// AuthPolicy targeting the same HTTPRoute is detected and reported.
func TestDetectConflictingAuthPolicies_RogueDetected(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
		rogueName      = "kserve-route-authn"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	rogueAP := newRogueAuthPolicy(rogueName, namespace, httpRouteName)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, rogueAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConflictingAuthPolicy=True, got %v", cond.Status)
	}
	if cond.Reason != "ConflictDetected" {
		t.Errorf("expected reason ConflictDetected, got %q", cond.Reason)
	}
	if !strings.Contains(cond.Message, rogueName) {
		t.Errorf("expected message to contain rogue policy name %q, got %q", rogueName, cond.Message)
	}
}

// TestDetectConflictingAuthPolicies_MultipleRogues verifies that multiple
// non-MaaS AuthPolicies are all detected and listed in the condition.
func TestDetectConflictingAuthPolicies_MultipleRogues(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	rogue1 := newRogueAuthPolicy("kserve-route-authn", namespace, httpRouteName)
	rogue2 := newRogueAuthPolicy("custom-auth-policy", namespace, httpRouteName)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, rogue1, rogue2).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConflictingAuthPolicy=True, got %v", cond.Status)
	}
	if !strings.Contains(cond.Message, "2 non-MaaS AuthPolicies") {
		t.Errorf("expected message to mention 2 policies, got %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "kserve-route-authn") {
		t.Errorf("expected message to contain 'kserve-route-authn', got %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "custom-auth-policy") {
		t.Errorf("expected message to contain 'custom-auth-policy', got %q", cond.Message)
	}
}

// TestDetectConflictingAuthPolicies_DifferentRoute verifies that a non-MaaS
// AuthPolicy targeting a DIFFERENT HTTPRoute is NOT flagged as a conflict.
func TestDetectConflictingAuthPolicies_DifferentRoute(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	unrelatedAP := newRogueAuthPolicy("other-policy", namespace, "completely-different-route")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, unrelatedAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False (different route), got %v", cond.Status)
	}
}

// TestDetectConflictingAuthPolicies_CrossNamespaceIsolation verifies that
// rogue AuthPolicies in a DIFFERENT namespace than the HTTPRoute are ignored.
func TestDetectConflictingAuthPolicies_CrossNamespaceIsolation(t *testing.T) {
	const (
		modelName      = "llm"
		modelNamespace = "model-ns"
		policyNS       = "policy-ns"
		otherNS        = "other-ns"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
	)

	model := newMaaSModelRef(modelName, modelNamespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, modelNamespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, policyNS, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: modelNamespace})
	rogueInOtherNS := newRogueAuthPolicy("rogue-policy", otherNS, httpRouteName)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, rogueInOtherNS).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: policyNS}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False (rogue in different namespace), got %v", cond.Status)
	}
}

// TestDetectConflictingAuthPolicies_ConflictResolved verifies that the
// condition transitions from True to False when the rogue policy is removed.
func TestDetectConflictingAuthPolicies_ConflictResolved(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
		rogueName      = "kserve-route-authn"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	rogueAP := newRogueAuthPolicy(rogueName, namespace, httpRouteName)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, rogueAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}

	// First reconcile: conflict detected
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(ctx, req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}
	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected initial ConflictingAuthPolicy=True, got %v", cond)
	}

	// Remove rogue policy
	if err := c.Delete(ctx, rogueAP); err != nil {
		t.Fatalf("Delete rogue policy: %v", err)
	}

	// Second reconcile: conflict resolved
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}

	if err := c.Get(ctx, req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy after resolution: %v", err)
	}
	cond = apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found after resolution")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False after conflict resolution, got %v", cond.Status)
	}
}

// TestDetectConflictingAuthPolicies_MissingModel verifies that conflict
// detection handles missing models gracefully (no false positives).
func TestDetectConflictingAuthPolicies_MissingModel(t *testing.T) {
	const (
		namespace      = "default"
		maasPolicyName = "policy-a"
	)

	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a",
		maasv1alpha1.ModelRef{Name: "non-existent-model", Namespace: namespace})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(maasPolicy).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False (model not found, no route to check), got %v", cond.Status)
	}
}

// TestDetectConflictingAuthPolicies_GatewayTarget verifies that a non-MaaS
// AuthPolicy targeting a Gateway (not HTTPRoute) is NOT flagged as a conflict
// on the model's HTTPRoute. Gateway-level policies are a separate concern.
func TestDetectConflictingAuthPolicies_GatewayTarget(t *testing.T) {
	const (
		modelName      = "llm"
		namespace      = "default"
		httpRouteName  = modelName
		maasPolicyName = "policy-a"
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a", maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})

	gatewayAP := &unstructured.Unstructured{}
	gatewayAP.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	gatewayAP.SetName("gateway-default-auth")
	gatewayAP.SetNamespace(namespace)
	_ = unstructured.SetNestedMap(gatewayAP.Object, map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  "maas-default-gateway",
		},
	}, "spec")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, maasPolicy, gatewayAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var policy maasv1alpha1.MaaSAuthPolicy
	if err := c.Get(context.Background(), req.NamespacedName, &policy); err != nil {
		t.Fatalf("Get MaaSAuthPolicy: %v", err)
	}

	cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
	if cond == nil {
		t.Fatal("ConflictingAuthPolicy condition not found")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConflictingAuthPolicy=False (Gateway target, not HTTPRoute), got %v", cond.Status)
	}
}

// TestDetectConflictingAuthPolicies_Deduplication verifies that when multiple
// modelRefs resolve to the same HTTPRoute, a single rogue AuthPolicy on that
// route is reported only once (not once per modelRef).
func TestDetectConflictingAuthPolicies_Deduplication(t *testing.T) {
	const (
		namespace      = "default"
		sharedRoute    = "shared-route"
		maasPolicyName = "policy-a"
		rogueName      = "kserve-route-authn"
	)

	modelA := newMaaSModelRef("model-a", namespace, "ExternalModel", sharedRoute)
	modelB := newMaaSModelRef("model-b", namespace, "ExternalModel", sharedRoute)
	route := newHTTPRoute(sharedRoute, namespace)
	maasPolicy := newMaaSAuthPolicy(maasPolicyName, namespace, "team-a",
		maasv1alpha1.ModelRef{Name: "model-a", Namespace: namespace},
		maasv1alpha1.ModelRef{Name: "model-b", Namespace: namespace},
	)
	rogueAP := newRogueAuthPolicy(rogueName, namespace, sharedRoute)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, modelB, route, maasPolicy, rogueAP).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme, MaaSAPINamespace: "maas-system"}
	log := ctrl.Log.WithName("test")

	conflicts, err := r.detectConflictingAuthPolicies(context.Background(), log, maasPolicy)
	if err != nil {
		t.Fatalf("detectConflictingAuthPolicies: unexpected error: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 deduplicated conflict, got %d: %v", len(conflicts), conflicts)
	}
	if conflicts[0].Name != rogueName {
		t.Errorf("expected conflict name %q, got %q", rogueName, conflicts[0].Name)
	}
}

// TestSetConflictingAuthPolicyCondition_Unit tests the condition-setting logic directly.
func TestSetConflictingAuthPolicyCondition_Unit(t *testing.T) {
	tests := []struct {
		name       string
		conflicts  []conflictingPolicyInfo
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "no conflicts",
			conflicts:  nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: "NoConflict",
		},
		{
			name: "one conflict",
			conflicts: []conflictingPolicyInfo{
				{Name: "rogue", Namespace: "ns", HTTPRouteName: "route", Model: "model", ModelNS: "ns"},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "ConflictDetected",
		},
		{
			name: "multiple conflicts",
			conflicts: []conflictingPolicyInfo{
				{Name: "rogue1", Namespace: "ns", HTTPRouteName: "route", Model: "model", ModelNS: "ns"},
				{Name: "rogue2", Namespace: "ns", HTTPRouteName: "route", Model: "model", ModelNS: "ns"},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "ConflictDetected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := &maasv1alpha1.MaaSAuthPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", Generation: 1},
			}
			setConflictingAuthPolicyCondition(policy, tc.conflicts)

			cond := apimeta.FindStatusCondition(policy.Status.Conditions, ConditionConflictingAuthPolicy)
			if cond == nil {
				t.Fatal("condition not set")
			}
			if cond.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", cond.Status, tc.wantStatus)
			}
			if cond.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", cond.Reason, tc.wantReason)
			}
			if cond.ObservedGeneration != 1 {
				t.Errorf("observedGeneration = %d, want 1", cond.ObservedGeneration)
			}
		})
	}
}

// TestPluralY verifies the pluralization helper.
func TestPluralY(t *testing.T) {
	if pluralY(1) != "y" {
		t.Errorf("pluralY(1) = %q, want \"y\"", pluralY(1))
	}
	if pluralY(2) != "ies" {
		t.Errorf("pluralY(2) = %q, want \"ies\"", pluralY(2))
	}
	if pluralY(0) != "ies" {
		t.Errorf("pluralY(0) = %q, want \"ies\"", pluralY(0))
	}
}
