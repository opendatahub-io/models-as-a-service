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

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestMaaSAuthPolicyReconciler_CrossNamespace verifies that MaaSAuthPolicy
// can reference models in different namespaces and generates AuthPolicies
// in the correct (model's) namespace.
func TestMaaSAuthPolicyReconciler_CrossNamespace(t *testing.T) {
	const (
		policyNamespace = "policy-ns"
		modelNamespaceA = "model-ns-a"
		modelNamespaceB = "model-ns-b"
		modelName       = "test-model"
		httpRouteName   = "maas-model-" + modelName
		authPolicyName  = "maas-auth-" + modelName
		maasPolicyName  = "cross-ns-policy"
	)

	// Model and HTTPRoute in namespace-a
	modelA := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceA},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeA := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceA},
	}

	// Model and HTTPRoute in namespace-b
	modelB := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceB},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeB := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceB},
	}

	// MaaSAuthPolicy in policy-ns referencing models in both namespace-a and namespace-b
	maasPolicy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: maasPolicyName, Namespace: policyNamespace},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{
				{Name: modelName, Namespace: modelNamespaceA},
				{Name: modelName, Namespace: modelNamespaceB},
			},
			Subjects: maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, routeA, modelB, routeB, maasPolicy).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: policyNamespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	// Verify AuthPolicy created in modelNamespaceA
	authPolicyA := &unstructured.Unstructured{}
	authPolicyA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: modelNamespaceA}, authPolicyA); err != nil {
		t.Errorf("AuthPolicy in namespace %q not found: %v", modelNamespaceA, err)
	} else {
		// Verify it targets the correct HTTPRoute
		targetRefName, _, _ := unstructured.NestedString(authPolicyA.Object, "spec", "targetRef", "name")
		if targetRefName != httpRouteName {
			t.Errorf("AuthPolicy in %q has targetRef.name = %q, want %q", modelNamespaceA, targetRefName, httpRouteName)
		}
	}

	// Verify AuthPolicy created in modelNamespaceB
	authPolicyB := &unstructured.Unstructured{}
	authPolicyB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: modelNamespaceB}, authPolicyB); err != nil {
		t.Errorf("AuthPolicy in namespace %q not found: %v", modelNamespaceB, err)
	} else {
		targetRefName, _, _ := unstructured.NestedString(authPolicyB.Object, "spec", "targetRef", "name")
		if targetRefName != httpRouteName {
			t.Errorf("AuthPolicy in %q has targetRef.name = %q, want %q", modelNamespaceB, targetRefName, httpRouteName)
		}
	}

	// Verify NO AuthPolicy created in the policy namespace
	wrongNsAuthPolicy := &unstructured.Unstructured{}
	wrongNsAuthPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: policyNamespace}, wrongNsAuthPolicy)
	if err == nil {
		t.Errorf("AuthPolicy should NOT be created in policy namespace %q, but it exists", policyNamespace)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error checking for AuthPolicy in policy namespace: %v", err)
	}
}

// TestMaaSAuthPolicyReconciler_SelectiveModelManagement verifies that when a
// MaaSAuthPolicy references only specific models, AuthPolicies are created only
// for those models, and not for unreferenced models in other namespaces.
func TestMaaSAuthPolicyReconciler_SelectiveModelManagement(t *testing.T) {
	const (
		policyNamespace = "policy-ns"
		modelNamespaceA = "model-ns-a"
		modelNamespaceB = "model-ns-b"
		modelName       = "test-model"
		httpRouteName   = "maas-model-" + modelName
		authPolicyName  = "maas-auth-" + modelName
		maasPolicyName  = "selective-policy"
	)

	// Model and HTTPRoute in namespace-a (referenced by policy)
	modelA := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceA},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeA := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceA},
	}

	// Model and HTTPRoute in namespace-b (NOT referenced by policy)
	modelB := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceB},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeB := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceB},
	}

	// MaaSAuthPolicy in policy-ns referencing ONLY modelA (not modelB)
	maasPolicy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: maasPolicyName, Namespace: policyNamespace},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{
				{Name: modelName, Namespace: modelNamespaceA}, // Only modelA
			},
			Subjects: maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, routeA, modelB, routeB, maasPolicy).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: maasPolicyName, Namespace: policyNamespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	// Verify AuthPolicy created in modelNamespaceA (referenced model)
	authPolicyA := &unstructured.Unstructured{}
	authPolicyA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: modelNamespaceA}, authPolicyA); err != nil {
		t.Errorf("AuthPolicy should exist in namespace %q (referenced model): %v", modelNamespaceA, err)
	}

	// Verify NO AuthPolicy created in modelNamespaceB (unreferenced model)
	authPolicyB := &unstructured.Unstructured{}
	authPolicyB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: modelNamespaceB}, authPolicyB)
	if !apierrors.IsNotFound(err) {
		t.Errorf("AuthPolicy should NOT exist in namespace %q (unreferenced model), but got: %v", modelNamespaceB, err)
	}
}

// TestMaaSAuthPolicyReconciler_SameNameDifferentNamespaces verifies that
// models with the same name in different namespaces are properly isolated.
func TestMaaSAuthPolicyReconciler_SameNameDifferentNamespaces(t *testing.T) {
	const (
		modelName      = "shared-model"
		namespaceA     = "team-a"
		namespaceB     = "team-b"
		httpRouteName  = "maas-model-" + modelName
		authPolicyName = "maas-auth-" + modelName
	)

	// Two models with same name in different namespaces
	modelA := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespaceA},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeA := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: namespaceA},
	}

	modelB := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespaceB},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeB := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: namespaceB},
	}

	// Policy for model in namespace-a only
	policyA := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: namespaceA},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{{Name: modelName, Namespace: namespaceA}},
			Subjects:  maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
		},
	}

	// Policy for model in namespace-b only
	policyB := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-b", Namespace: namespaceB},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: []maasv1alpha1.ModelRef{{Name: modelName, Namespace: namespaceB}},
			Subjects:  maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-b"}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, routeA, modelB, routeB, policyA, policyB).
		WithStatusSubresource(&maasv1alpha1.MaaSAuthPolicy{}).
		Build()

	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme}

	// Reconcile both policies
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: "policy-a", Namespace: namespaceA}}
	if _, err := r.Reconcile(context.Background(), reqA); err != nil {
		t.Fatalf("Reconcile policy-a: unexpected error: %v", err)
	}

	reqB := ctrl.Request{NamespacedName: types.NamespacedName{Name: "policy-b", Namespace: namespaceB}}
	if _, err := r.Reconcile(context.Background(), reqB); err != nil {
		t.Fatalf("Reconcile policy-b: unexpected error: %v", err)
	}

	// Verify AuthPolicy exists in namespace-a
	authPolicyA := &unstructured.Unstructured{}
	authPolicyA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: namespaceA}, authPolicyA); err != nil {
		t.Errorf("AuthPolicy in namespace %q not found: %v", namespaceA, err)
	}

	// Verify AuthPolicy exists in namespace-b
	authPolicyB := &unstructured.Unstructured{}
	authPolicyB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: namespaceB}, authPolicyB); err != nil {
		t.Errorf("AuthPolicy in namespace %q not found: %v", namespaceB, err)
	}

	// Verify they're separate resources (checking subjects would require parsing the spec)
	if authPolicyA.GetNamespace() == authPolicyB.GetNamespace() {
		t.Errorf("AuthPolicies should be in different namespaces, both in: %q", authPolicyA.GetNamespace())
	}
}

// TestMaaSSubscriptionReconciler_CrossNamespace verifies that MaaSSubscription
// can reference models in different namespaces and generates TokenRateLimitPolicies
// in the correct (model's) namespace.
func TestMaaSSubscriptionReconciler_CrossNamespace(t *testing.T) {
	const (
		subNamespace    = "subscription-ns"
		modelNamespaceA = "model-ns-a"
		modelNamespaceB = "model-ns-b"
		modelName       = "test-model"
		httpRouteName   = "maas-model-" + modelName
		trlpName        = "maas-trlp-" + modelName
		subName         = "cross-ns-subscription"
	)

	// Model and HTTPRoute in namespace-a
	modelA := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceA},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeA := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceA},
	}

	// Model and HTTPRoute in namespace-b
	modelB := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: modelNamespaceB},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}
	routeB := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: httpRouteName, Namespace: modelNamespaceB},
	}

	// MaaSSubscription in subscription-ns referencing models in both namespaces
	maasSub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: subName, Namespace: subNamespace},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner: maasv1alpha1.OwnerSpec{
				Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}},
			},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
				{
					Name:            modelName,
					Namespace:       modelNamespaceA,
					TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}},
				},
				{
					Name:            modelName,
					Namespace:       modelNamespaceB,
					TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 200, Window: "1m"}},
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, routeA, modelB, routeB, maasSub).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: subName, Namespace: subNamespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}

	// Verify TokenRateLimitPolicy created in modelNamespaceA
	trlpA := &unstructured.Unstructured{}
	trlpA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: trlpName, Namespace: modelNamespaceA}, trlpA); err != nil {
		t.Errorf("TokenRateLimitPolicy in namespace %q not found: %v", modelNamespaceA, err)
	}

	// Verify TokenRateLimitPolicy created in modelNamespaceB
	trlpB := &unstructured.Unstructured{}
	trlpB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: trlpName, Namespace: modelNamespaceB}, trlpB); err != nil {
		t.Errorf("TokenRateLimitPolicy in namespace %q not found: %v", modelNamespaceB, err)
	}

	// Verify NO TokenRateLimitPolicy created in the subscription namespace
	wrongNsTRLP := &unstructured.Unstructured{}
	wrongNsTRLP.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	err := c.Get(context.Background(), types.NamespacedName{Name: trlpName, Namespace: subNamespace}, wrongNsTRLP)
	if err == nil {
		t.Errorf("TokenRateLimitPolicy should NOT be created in subscription namespace %q, but it exists", subNamespace)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error checking for TRLP in subscription namespace: %v", err)
	}
}

// TestMaaSModelRefDeletion_CrossNamespaceIsolation verifies that deleting
// a model in one namespace doesn't affect a same-named model in another namespace.
func TestMaaSModelRefDeletion_CrossNamespaceIsolation(t *testing.T) {
	const (
		modelName      = "shared-model"
		namespaceA     = "team-a"
		namespaceB     = "team-b"
		httpRouteName  = "maas-model-" + modelName
		authPolicyName = "maas-auth-" + modelName
	)

	// Two models with same name in different namespaces
	modelA := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{
			Name:       modelName,
			Namespace:  namespaceA,
			Finalizers: []string{maasModelFinalizer},
		},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}

	modelB := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespaceB},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: modelName},
		},
	}

	// AuthPolicies for both models
	authPolicyA := newPreexistingAuthPolicy(authPolicyName, namespaceA, modelName, nil)
	authPolicyB := newPreexistingAuthPolicy(authPolicyName, namespaceB, modelName, nil)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(modelA, modelB, authPolicyA, authPolicyB).
		WithStatusSubresource(&maasv1alpha1.MaaSModelRef{}).
		Build()

	// Delete modelA
	if err := c.Delete(context.Background(), modelA); err != nil {
		t.Fatalf("Delete modelA: %v", err)
	}

	// Reconcile deletion of modelA
	r := &MaaSModelRefReconciler{Client: c, Scheme: scheme}
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: namespaceA}}
	if _, err := r.Reconcile(context.Background(), reqA); err != nil {
		t.Fatalf("Reconcile modelA deletion: unexpected error: %v", err)
	}

	// Verify authPolicyA was deleted
	gotA := &unstructured.Unstructured{}
	gotA.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	errA := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: namespaceA}, gotA)
	if !apierrors.IsNotFound(errA) {
		t.Errorf("AuthPolicy in namespace %q should be deleted, but it still exists or got error: %v", namespaceA, errA)
	}

	// Verify authPolicyB still exists (isolation)
	gotB := &unstructured.Unstructured{}
	gotB.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: authPolicyName, Namespace: namespaceB}, gotB); err != nil {
		t.Errorf("AuthPolicy in namespace %q should NOT be deleted when model in %q is deleted, but got error: %v", namespaceB, namespaceA, err)
	}

	// Verify modelB still exists
	stillExists := &maasv1alpha1.MaaSModelRef{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: modelName, Namespace: namespaceB}, stillExists); err != nil {
		t.Errorf("Model in namespace %q should NOT be affected by deletion in %q, but got error: %v", namespaceB, namespaceA, err)
	}
}
