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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newMaaSPlatformAuth(name, ns string, oidc *maasv1alpha1.ExternalOIDCConfig) *maasv1alpha1.MaaSPlatformAuth {
	return &maasv1alpha1.MaaSPlatformAuth{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       maasv1alpha1.MaaSPlatformAuthSpec{ExternalOIDC: oidc},
	}
}

// newBaseAuthPolicy creates a minimal AuthPolicy as kustomize would deploy it.
func newBaseAuthPolicy(namespace string) *unstructured.Unstructured {
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	ap.SetName(maasAPIAuthPolicyName)
	ap.SetNamespace(namespace)
	_ = unstructured.SetNestedField(ap.Object, maasAPIRouteName, "spec", "targetRef", "name")
	return ap
}

func TestMaaSPlatformAuth_AddsOIDCToExistingAuthPolicy(t *testing.T) {
	pa := newMaaSPlatformAuth("platform-auth", "opendatahub", &maasv1alpha1.ExternalOIDCConfig{
		IssuerURL: "https://keycloak.example.com/realms/maas",
		ClientID:  "maas-cli",
		TTL:       300,
	})
	existingAP := newBaseAuthPolicy("opendatahub")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa, existingAP).
		WithStatusSubresource(&maasv1alpha1.MaaSPlatformAuth{}).
		Build()

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
		ClusterAudience:  "https://kubernetes.default.svc",
		GatewayName:      "maas-default-gateway",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap); err != nil {
		t.Fatalf("AuthPolicy not found: %v", err)
	}

	// OIDC rules should be present
	oidcAuth, found, _ := unstructured.NestedMap(ap.Object, "spec", "rules", "authentication", "oidc-identities")
	if !found {
		t.Fatal("oidc-identities not found after reconcile with OIDC config")
	}
	issuer, _, _ := unstructured.NestedString(oidcAuth, "jwt", "issuerUrl")
	if issuer != "https://keycloak.example.com/realms/maas" {
		t.Errorf("issuerUrl = %q, want %q", issuer, "https://keycloak.example.com/realms/maas")
	}

	// OpenShift priority bumped to 2
	priority, found, _ := unstructured.NestedFieldNoCopy(ap.Object, "spec", "rules", "authentication", "openshift-identities", "priority")
	if !found || priority != int64(2) {
		t.Errorf("openshift-identities.priority = %v, want 2", priority)
	}

	// OIDC client binding authorization present
	_, found, _ = unstructured.NestedMap(ap.Object, "spec", "rules", "authorization", "oidc-client-bound")
	if !found {
		t.Error("oidc-client-bound authorization not found")
	}
}

func TestMaaSPlatformAuth_WithoutOIDCKeepsBaseRules(t *testing.T) {
	pa := newMaaSPlatformAuth("platform-auth", "opendatahub", nil)
	existingAP := newBaseAuthPolicy("opendatahub")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa, existingAP).
		WithStatusSubresource(&maasv1alpha1.MaaSPlatformAuth{}).
		Build()

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
		ClusterAudience:  "https://kubernetes.default.svc",
		GatewayName:      "maas-default-gateway",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap); err != nil {
		t.Fatalf("AuthPolicy not found: %v", err)
	}

	// No OIDC rules
	_, found, _ := unstructured.NestedMap(ap.Object, "spec", "rules", "authentication", "oidc-identities")
	if found {
		t.Error("oidc-identities should not be present without OIDC config")
	}

	// OpenShift priority stays at 1
	priority, _, _ := unstructured.NestedFieldNoCopy(ap.Object, "spec", "rules", "authentication", "openshift-identities", "priority")
	if priority != int64(1) {
		t.Errorf("openshift-identities.priority = %v, want 1", priority)
	}

	// API keys still present
	_, found, _ = unstructured.NestedMap(ap.Object, "spec", "rules", "authentication", "api-keys")
	if !found {
		t.Error("api-keys authentication should be present")
	}
}

func TestMaaSPlatformAuth_DeletionRestoresBaseState(t *testing.T) {
	pa := &maasv1alpha1.MaaSPlatformAuth{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "platform-auth",
			Namespace:  "opendatahub",
			Finalizers: []string{maasPlatformAuthFinalizer},
		},
		Spec: maasv1alpha1.MaaSPlatformAuthSpec{
			ExternalOIDC: &maasv1alpha1.ExternalOIDCConfig{
				IssuerURL: "https://keycloak.example.com/realms/maas",
				ClientID:  "maas-cli",
				TTL:       300,
			},
		},
	}
	existingAP := newBaseAuthPolicy("opendatahub")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa, existingAP).
		Build()

	if err := c.Delete(context.Background(), pa); err != nil {
		t.Fatalf("Delete MaaSPlatformAuth: %v", err)
	}

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
		ClusterAudience:  "https://kubernetes.default.svc",
		GatewayName:      "maas-default-gateway",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile (deletion): %v", err)
	}

	// AuthPolicy should still exist but without OIDC
	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap); err != nil {
		t.Fatalf("AuthPolicy should still exist after CR deletion: %v", err)
	}

	_, found, _ := unstructured.NestedMap(ap.Object, "spec", "rules", "authentication", "oidc-identities")
	if found {
		t.Error("oidc-identities should be removed after CR deletion")
	}

	priority, _, _ := unstructured.NestedFieldNoCopy(ap.Object, "spec", "rules", "authentication", "openshift-identities", "priority")
	if priority != int64(1) {
		t.Errorf("openshift-identities.priority = %v, want 1 after restoration", priority)
	}
}

func TestMaaSPlatformAuth_NoopUpdateSkipped(t *testing.T) {
	pa := newMaaSPlatformAuth("platform-auth", "opendatahub", nil)
	existingAP := newBaseAuthPolicy("opendatahub")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa, existingAP).
		WithStatusSubresource(&maasv1alpha1.MaaSPlatformAuth{}).
		Build()

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
		ClusterAudience:  "https://kubernetes.default.svc",
		GatewayName:      "maas-default-gateway",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}

	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap); err != nil {
		t.Fatalf("AuthPolicy not found: %v", err)
	}
	rv1 := ap.GetResourceVersion()

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}

	ap2 := &unstructured.Unstructured{}
	ap2.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap2); err != nil {
		t.Fatalf("AuthPolicy not found: %v", err)
	}
	if rv1 != ap2.GetResourceVersion() {
		t.Errorf("redundant update: ResourceVersion changed from %s to %s", rv1, ap2.GetResourceVersion())
	}
}

func TestMaaSPlatformAuth_ClusterAudienceConfigured(t *testing.T) {
	pa := newMaaSPlatformAuth("platform-auth", "opendatahub", nil)
	existingAP := newBaseAuthPolicy("opendatahub")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa, existingAP).
		WithStatusSubresource(&maasv1alpha1.MaaSPlatformAuth{}).
		Build()

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
		ClusterAudience:  "https://oidc.hypershift.example.com",
		GatewayName:      "my-gateway",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	ap := &unstructured.Unstructured{}
	ap.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	if err := c.Get(context.Background(), types.NamespacedName{Name: maasAPIAuthPolicyName, Namespace: "opendatahub"}, ap); err != nil {
		t.Fatalf("AuthPolicy not found: %v", err)
	}

	audiences, found, _ := unstructured.NestedSlice(
		ap.Object,
		"spec", "rules", "authentication", "openshift-identities", "kubernetesTokenReview", "audiences",
	)
	if !found || len(audiences) != 2 {
		t.Fatalf("audiences not found or wrong length: found=%v len=%d", found, len(audiences))
	}
	if audiences[0] != "https://oidc.hypershift.example.com" {
		t.Errorf("audiences[0] = %v, want %v", audiences[0], "https://oidc.hypershift.example.com")
	}
	if audiences[1] != "my-gateway-sa" {
		t.Errorf("audiences[1] = %v, want %q", audiences[1], "my-gateway-sa")
	}
}

func TestMaaSPlatformAuth_ReturnsErrorWhenAuthPolicyMissing(t *testing.T) {
	pa := newMaaSPlatformAuth("platform-auth", "opendatahub", nil)
	// No existing AuthPolicy — kustomize hasn't deployed it yet

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(pa).
		WithStatusSubresource(&maasv1alpha1.MaaSPlatformAuth{}).
		Build()

	r := &MaaSPlatformAuthReconciler{
		Client: c, Scheme: scheme,
		MaaSAPINamespace: "opendatahub",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "platform-auth", Namespace: "opendatahub"}}
	_, err := r.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("Reconcile should return error when AuthPolicy doesn't exist yet")
	}
}
