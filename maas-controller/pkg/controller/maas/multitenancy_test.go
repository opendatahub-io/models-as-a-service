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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func TestIsTenantNamespace_DefaultNamespace(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if !isTenantNamespace(context.Background(), c, "models-as-a-service", "models-as-a-service", false) {
		t.Error("default tenant namespace should be recognized")
	}
}

func TestIsTenantNamespace_LabeledNamespace(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{
			name:   "ADR tenant label",
			labels: map[string]string{tenantreconcile.LabelAIGatewayTenant: "team-a"},
		},
		{
			name:   "AITenant compatibility label",
			labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "team-a-maas",
					Labels: tt.labels,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
			if !isTenantNamespace(context.Background(), c, "team-a-maas", "models-as-a-service", true) {
				t.Error("labeled namespace should be recognized as tenant namespace")
			}
			if isTenantNamespace(context.Background(), c, "team-a-maas", "models-as-a-service", false) {
				t.Error("labeled namespace should be ignored while discovery is disabled")
			}
		})
	}
}

func TestIsTenantNamespace_UnlabeledNamespace(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "random-ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	if isTenantNamespace(context.Background(), c, "random-ns", "models-as-a-service", true) {
		t.Error("unlabeled namespace should NOT be recognized as tenant namespace")
	}
}

func TestIsTenantNamespace_EmptyDefault(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if !isTenantNamespace(context.Background(), c, "any-ns", "", false) {
		t.Error("when default is empty all namespaces should be accepted (backward compat)")
	}
}

func TestMaaSAuthPolicyReconciler_FetchTenantIdentifierRejectsInvalidDefaultLabels(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: "ai-tenant-spoofed",
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantreconcile.DefaultAITenantName,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()
	r := &MaaSAuthPolicyReconciler{Client: c, Scheme: scheme}

	tenantID, err := r.fetchTenantIdentifier(context.Background(), ctrl.Log, tenant.Namespace)
	if err == nil {
		t.Fatalf("fetchTenantIdentifier error = nil, want invalid default tenant label error")
	}
	if tenantID != "" {
		t.Fatalf("tenantID = %q, want empty on error", tenantID)
	}
}

func TestMaaSAuthPolicyReconciler_IgnoresNonTenantNamespace(t *testing.T) {
	const (
		namespace  = "random-ns"
		policyName = "test-policy"
		modelName  = "llm"
	)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newExternalModelHTTPRoute(modelName, namespace)
	policy := newMaaSAuthPolicy(policyName, namespace, "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(ns, model, route, policy).
		Build()

	r := &MaaSAuthPolicyReconciler{
		Client:                          c,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: namespace}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Error("expected no requeue for non-tenant namespace")
	}

	authPolicy := &unstructured.Unstructured{}
	authPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	err = c.Get(context.Background(), types.NamespacedName{Name: "maas-auth-" + modelName, Namespace: namespace}, authPolicy)
	if !apierrors.IsNotFound(err) {
		t.Errorf("AuthPolicy should NOT be created for non-tenant namespace, got err: %v", err)
	}
}

func TestMaaSAuthPolicyReconciler_ReconcilesTenantNamespace(t *testing.T) {
	const (
		namespace  = "team-a-maas"
		policyName = "test-policy"
		modelName  = "llm"
	)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: namespace},
		Spec: maasv1alpha1.TenantSpec{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "team-a-gateway",
			Namespace: "team-a-gateway-ns",
		}},
	}
	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRouteWithGateway(modelName, namespace, "team-a-gateway", "team-a-gateway-ns")
	policy := newMaaSAuthPolicy(policyName, namespace, "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(ns, tenant, model, route, policy).
		Build()

	const gwNamespace = "team-a-gateway-ns"
	r := &MaaSAuthPolicyReconciler{
		Client:                          c,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
		// Controller's default gateway (different from tenant's gateway)
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "openshift-ingress",
		InfraNamespace:   "opendatahub",
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: namespace}}
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	gatewayAP := &unstructured.Unstructured{}
	gatewayAP.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	// Gateway AuthPolicy name is now dynamic: {gatewayName}-maas-auth
	expectedAuthPolicyName := "team-a-gateway-maas-auth"
	if err := c.Get(context.Background(), types.NamespacedName{Name: expectedAuthPolicyName, Namespace: gwNamespace}, gatewayAP); err != nil {
		t.Errorf("Gateway AuthPolicy should be created for AITenant-labeled tenant namespace: %v", err)
	}
}

func TestMaaSSubscriptionReconciler_IgnoresNonTenantNamespace(t *testing.T) {
	const (
		namespace = "random-ns"
		subName   = "test-sub"
		modelName = "llm"
	)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newExternalModelHTTPRoute(modelName, namespace)
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: subName, Namespace: namespace},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{Name: modelName, Namespace: namespace}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(ns, model, route, sub).
		Build()

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: subName, Namespace: namespace}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Error("expected no requeue for non-tenant namespace")
	}
}

func TestMaaSAuthPolicyReconciler_DeletionRunsAfterNamespaceDelabeled(t *testing.T) {
	const (
		namespace  = "team-a-maas"
		policyName = "test-policy"
		modelName  = "llm"
	)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	policy := newMaaSAuthPolicy(policyName, namespace, "team-a",
		maasv1alpha1.ModelRef{Name: modelName, Namespace: namespace})
	policy.Finalizers = []string{maasAuthPolicyFinalizer}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(ns, policy).
		Build()

	if err := c.Delete(context.Background(), policy); err != nil {
		t.Fatalf("delete MaaSAuthPolicy: %v", err)
	}

	r := &MaaSAuthPolicyReconciler{
		Client:                          c,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := &maasv1alpha1.MaaSAuthPolicy{}
	err := c.Get(context.Background(), req.NamespacedName, got)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatalf("get MaaSAuthPolicy after deletion reconcile: %v", err)
	}
	if len(got.Finalizers) != 0 {
		t.Fatalf("expected finalizers removed after deletion in delabeled namespace, got %v", got.Finalizers)
	}
}

func TestMaaSSubscriptionReconciler_DeletionRunsAfterNamespaceDelabeled(t *testing.T) {
	const (
		namespace = "team-a-maas"
		subName   = "test-sub"
		modelName = "llm"
	)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	sub := newMaaSSubscription(subName, namespace, "team-a", modelName, 100)
	sub.Finalizers = []string{maasSubscriptionFinalizer}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(ns, sub).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()

	if err := c.Delete(context.Background(), sub); err != nil {
		t.Fatalf("delete MaaSSubscription: %v", err)
	}

	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: subName, Namespace: namespace}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := &maasv1alpha1.MaaSSubscription{}
	err := c.Get(context.Background(), req.NamespacedName, got)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatalf("get MaaSSubscription after deletion reconcile: %v", err)
	}
	if len(got.Finalizers) != 0 {
		t.Fatalf("expected finalizers removed after deletion in delabeled namespace, got %v", got.Finalizers)
	}
}

func TestMapTenantToMaaSAuthPolicies_ScopedToNamespace(t *testing.T) {
	const (
		tenantNSA = "tenant-a"
		tenantNSB = "tenant-b"
	)

	policyA := newMaaSAuthPolicy("policy-a", tenantNSA, "team-a",
		maasv1alpha1.ModelRef{Name: "llm", Namespace: "models"})
	policyB := newMaaSAuthPolicy("policy-b", tenantNSB, "team-b",
		maasv1alpha1.ModelRef{Name: "llm", Namespace: "models"})
	nsA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   tenantNSA,
		Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
	}}
	nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   tenantNSB,
		Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nsA, nsB, policyA, policyB).
		Build()

	r := &MaaSAuthPolicyReconciler{
		Client:                          c,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	tenantA := &unstructured.Unstructured{}
	tenantA.SetGroupVersionKind(schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "Tenant"})
	tenantA.SetName("default-tenant")
	tenantA.SetNamespace(tenantNSA)

	requests := r.mapTenantToMaaSAuthPolicies(context.Background(), tenantA)

	for _, req := range requests {
		if req.Namespace != tenantNSA {
			t.Errorf("Tenant change in %q should only enqueue policies from %q, got %q",
				tenantNSA, tenantNSA, req.Namespace)
		}
	}
	if len(requests) != 1 {
		t.Errorf("expected 1 request for tenant-a, got %d", len(requests))
	}
}

func TestFetchOIDCConfig_PerTenantNamespace(t *testing.T) {
	tenantA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "Tenant",
			"metadata": map[string]any{
				"name":      "default-tenant",
				"namespace": "tenant-a",
			},
			"spec": map[string]any{
				"externalOIDC": map[string]any{
					"issuerUrl": "https://idp-a.example.com",
					"clientId":  "client-a",
				},
			},
		},
	}
	tenantB := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "Tenant",
			"metadata": map[string]any{
				"name":      "default-tenant",
				"namespace": "tenant-b",
			},
			"spec": map[string]any{
				"externalOIDC": map[string]any{
					"issuerUrl": "https://idp-b.example.com",
					"clientId":  "client-b",
				},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenantA, tenantB).Build()
	r := &MaaSAuthPolicyReconciler{
		Client:          c,
		Scheme:          scheme,
		TenantNamespace: "models-as-a-service",
	}

	cfgA := r.fetchOIDCConfig(context.Background(), ctrl.Log, "tenant-a")
	if cfgA == nil {
		t.Fatal("expected OIDC config from tenant-a")
	}
	if cfgA.IssuerURL != "https://idp-a.example.com" {
		t.Errorf("tenant-a issuerURL = %q, want https://idp-a.example.com", cfgA.IssuerURL)
	}
	if cfgA.ClientID != "client-a" {
		t.Errorf("tenant-a clientID = %q, want client-a", cfgA.ClientID)
	}

	cfgB := r.fetchOIDCConfig(context.Background(), ctrl.Log, "tenant-b")
	if cfgB == nil {
		t.Fatal("expected OIDC config from tenant-b")
	}
	if cfgB.IssuerURL != "https://idp-b.example.com" {
		t.Errorf("tenant-b issuerURL = %q, want https://idp-b.example.com", cfgB.IssuerURL)
	}
}

func TestMapNamespaceToMaaSAuthPolicies_EnqueuesWhenDiscoveryEnabled(t *testing.T) {
	labeledPolicy := newMaaSAuthPolicy("policy-a", "tenant-a", "team-a",
		maasv1alpha1.ModelRef{Name: "llm", Namespace: "models"})
	unlabeledPolicy := newMaaSAuthPolicy("policy-b", "random-ns", "team-b",
		maasv1alpha1.ModelRef{Name: "llm", Namespace: "models"})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(labeledPolicy, unlabeledPolicy).Build()
	r := &MaaSAuthPolicyReconciler{
		Client:                          c,
		Scheme:                          scheme,
		TenantNamespace:                 "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	labeled := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "tenant-a",
			Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	requests := r.mapNamespaceToMaaSAuthPolicies(context.Background(), labeled)
	if len(requests) != 1 {
		t.Errorf("labeled namespace should enqueue policies, got %d requests", len(requests))
	}

	unlabeled := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "random-ns"},
	}
	requests = r.mapNamespaceToMaaSAuthPolicies(context.Background(), unlabeled)
	if len(requests) != 1 {
		t.Errorf("unlabeled namespace should enqueue policies on label changes when discovery is enabled, got %d requests", len(requests))
	}

	r.TenantNamespaceDiscoveryEnabled = false
	requests = r.mapNamespaceToMaaSAuthPolicies(context.Background(), unlabeled)
	if len(requests) != 0 {
		t.Errorf("unlabeled namespace should not enqueue policies when discovery is disabled, got %d requests", len(requests))
	}
}

func TestMapNamespaceToMaaSSubscriptions_EnqueuesWhenDiscoveryEnabled(t *testing.T) {
	labeledSub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-a", Namespace: "tenant-a"},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{Name: "llm", Namespace: "models"}},
		},
	}
	unlabeledSub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-b", Namespace: "random-ns"},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{Name: "llm", Namespace: "models"}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(labeledSub, unlabeledSub).Build()
	r := &MaaSSubscriptionReconciler{
		Client:                          c,
		Scheme:                          scheme,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: true,
	}

	labeled := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "tenant-a",
			Labels: map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	requests := r.mapNamespaceToMaaSSubscriptions(context.Background(), labeled)
	if len(requests) != 1 {
		t.Errorf("labeled namespace should enqueue subscriptions, got %d requests", len(requests))
	}

	unlabeled := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "random-ns"},
	}
	requests = r.mapNamespaceToMaaSSubscriptions(context.Background(), unlabeled)
	if len(requests) != 1 {
		t.Errorf("unlabeled namespace should enqueue subscriptions on label changes when discovery is enabled, got %d requests", len(requests))
	}

	r.TenantNamespaceDiscoveryEnabled = false
	requests = r.mapNamespaceToMaaSSubscriptions(context.Background(), unlabeled)
	if len(requests) != 0 {
		t.Errorf("unlabeled namespace should not enqueue subscriptions when discovery is disabled, got %d requests", len(requests))
	}
}

func TestFetchTenantForNamespace(t *testing.T) {
	tenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-tenant",
			Namespace: "team-a-maas",
		},
		Spec: maasv1alpha1.TenantSpec{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "team-a-gateway",
				Namespace: "openshift-ingress",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).Build()

	got, err := fetchTenantForNamespace(context.Background(), c, "team-a-maas")
	if err != nil {
		t.Fatalf("fetchTenantForNamespace: %v", err)
	}
	if got.Spec.GatewayRef.Name != "team-a-gateway" {
		t.Errorf("GatewayRef.Name = %q, want team-a-gateway", got.Spec.GatewayRef.Name)
	}

	_, err = fetchTenantForNamespace(context.Background(), c, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent namespace")
	}
}

func TestTenantGatewayRefForNamespace(t *testing.T) {
	const (
		defaultNamespace = "models-as-a-service"
		fallbackName     = "default-gateway"
		fallbackNS       = "openshift-ingress"
		tenantNamespace  = "team-a-maas"
	)

	fullTenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNamespace},
		Spec: maasv1alpha1.TenantSpec{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "team-a-gateway",
			Namespace: "team-a-gateway-ns",
		}},
	}
	defaultTenantWithoutRef := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: defaultNamespace},
	}
	partialTenant := &maasv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.TenantInstanceName, Namespace: "partial-ns"},
		Spec: maasv1alpha1.TenantSpec{GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name: "partial-gateway",
		}},
	}

	tests := []struct {
		name             string
		tenantNamespace  string
		discoveryEnabled bool
		objects          []client.Object
		want             maasv1alpha1.TenantGatewayRef
		wantErr          bool
	}{
		{
			name:             "tenant with full gatewayRef",
			tenantNamespace:  tenantNamespace,
			discoveryEnabled: true,
			objects:          []client.Object{fullTenant},
			want: maasv1alpha1.TenantGatewayRef{
				Name:      "team-a-gateway",
				Namespace: "team-a-gateway-ns",
			},
		},
		{
			name:             "default tenant with empty gatewayRef falls back to flags",
			tenantNamespace:  defaultNamespace,
			discoveryEnabled: true,
			objects:          []client.Object{defaultTenantWithoutRef},
			want: maasv1alpha1.TenantGatewayRef{
				Name:      fallbackName,
				Namespace: fallbackNS,
			},
		},
		{
			name:             "partial gatewayRef is invalid",
			tenantNamespace:  "partial-ns",
			discoveryEnabled: true,
			objects:          []client.Object{partialTenant},
			wantErr:          true,
		},
		{
			name:             "missing default tenant falls back to flags",
			tenantNamespace:  defaultNamespace,
			discoveryEnabled: true,
			want: maasv1alpha1.TenantGatewayRef{
				Name:      fallbackName,
				Namespace: fallbackNS,
			},
		},
		{
			name:             "missing discovered tenant is an error",
			tenantNamespace:  tenantNamespace,
			discoveryEnabled: true,
			wantErr:          true,
		},
		{
			name:             "discovery disabled uses legacy fallback",
			tenantNamespace:  tenantNamespace,
			discoveryEnabled: false,
			want: maasv1alpha1.TenantGatewayRef{
				Name:      fallbackName,
				Namespace: fallbackNS,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			got, err := tenantGatewayRefForNamespace(context.Background(), c, tt.tenantNamespace, defaultNamespace, fallbackName, fallbackNS, tt.discoveryEnabled)
			if (err != nil) != tt.wantErr {
				t.Fatalf("tenantGatewayRefForNamespace error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("tenantGatewayRefForNamespace = %#v, want %#v", got, tt.want)
			}
		})
	}
}
