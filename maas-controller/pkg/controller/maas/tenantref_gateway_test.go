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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const testAITenantNamespace = "ai-tenants"

func TestResolveGatewayRef_WithTenantRef(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "redteam",
			Namespace: testAITenantNamespace,
		},
		Spec: maasv1alpha1.AITenantSpec{},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "redteam-gateway",
				Namespace: "openshift-ingress",
			},
		},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "redteam",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()
	// Patch status since fake client WithObjects doesn't set status
	aitenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "redteam-gateway",
			Namespace: "openshift-ingress",
		},
	}
	if err := c.Status().Update(ctx, aitenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	ref, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if ref.Name != "redteam-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "redteam-gateway")
	}
	if ref.Namespace != "openshift-ingress" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "openshift-ingress")
	}
	if model.Status.ResolvedTenantRef != "redteam" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "redteam")
	}
}

func TestResolveGatewayRef_WithTenantRef_NotFound(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "nonexistent",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	_, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for nonexistent AITenant, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'not found'", err)
	}
}

func TestResolveGatewayRef_WithTenantRef_NoGatewayInStatus(t *testing.T) {
	ctx := context.Background()

	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-tenant",
			Namespace: testAITenantNamespace,
		},
		Spec:   maasv1alpha1.AITenantSpec{},
		Status: maasv1alpha1.AITenantStatus{},
	}

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "model-ns"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "empty-tenant",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(aitenant).
		Build()

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	_, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err == nil {
		t.Fatal("resolveGatewayRef() expected error for AITenant without gateway in status, got nil")
	}
	if !strings.Contains(err.Error(), "no gateway reference") {
		t.Errorf("resolveGatewayRef() error = %v, want error containing 'no gateway reference'", err)
	}
}

func TestResolveGatewayRef_WithoutTenantRef_FallsBackToDefault(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "models-as-a-service"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			// TenantRef is empty -- should fall back to default behavior
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:                          c,
		Scheme:                          scheme,
		GatewayName:                     testGatewayName,
		GatewayNamespace:                testGatewayNamespace,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: false,
		AITenantNamespace:               testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	ref, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	// When no tenant config exists and namespace is the default, should get the fallback gateway
	if ref.Name != testGatewayName {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, testGatewayName)
	}
	if ref.Namespace != testGatewayNamespace {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, testGatewayNamespace)
	}
}

func TestResolveGatewayRef_WithoutTenantRef_ClearsResolvedTenantRef(t *testing.T) {
	ctx := context.Background()

	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "models-as-a-service"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
		},
		Status: maasv1alpha1.MaaSModelStatus{
			ResolvedTenantRef: "old-tenant",
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MaaSModelRefReconciler{
		Client:                          c,
		Scheme:                          scheme,
		GatewayName:                     testGatewayName,
		GatewayNamespace:                testGatewayNamespace,
		DefaultTenantNamespace:          "models-as-a-service",
		TenantNamespaceDiscoveryEnabled: false,
		AITenantNamespace:               testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	_, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	if model.Status.ResolvedTenantRef != "" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want empty (stale value not cleared)", model.Status.ResolvedTenantRef)
	}
}

func TestResolveGatewayRef_WithTenantRef_OverridesModelNamespace(t *testing.T) {
	// This test verifies the core bug fix: even when model.Namespace points
	// to a different tenant, the tenantRef-based resolution uses the correct AITenant.
	ctx := context.Background()

	// AITenant for the correct tenant
	correctTenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "correct-tenant",
			Namespace: testAITenantNamespace,
		},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{
				Name:      "correct-gateway",
				Namespace: "correct-ns",
			},
		},
	}

	// Model is in a different namespace than the tenant it references
	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "some-other-namespace"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef:  maasv1alpha1.ModelReference{Kind: "LLMInferenceService", Name: "test-llmisvc"},
			TenantRef: "correct-tenant",
		},
	}

	objs := []client.Object{correctTenant}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.AITenant{}).
		Build()

	// Patch status
	correctTenant.Status = maasv1alpha1.AITenantStatus{
		GatewayRef: maasv1alpha1.TenantGatewayRef{
			Name:      "correct-gateway",
			Namespace: "correct-ns",
		},
	}
	if err := c.Status().Update(ctx, correctTenant); err != nil {
		t.Fatalf("failed to update AITenant status: %v", err)
	}

	r := &MaaSModelRefReconciler{
		Client:            c,
		Scheme:            scheme,
		GatewayName:       testGatewayName,
		GatewayNamespace:  testGatewayNamespace,
		AITenantNamespace: testAITenantNamespace,
	}
	h := &llmisvcHandler{r: r}

	ref, err := h.resolveGatewayRef(ctx, logr.Discard(), model)
	if err != nil {
		t.Fatalf("resolveGatewayRef() error = %v", err)
	}
	// Gateway should come from the AITenant, NOT from model.Namespace
	if ref.Name != "correct-gateway" {
		t.Errorf("resolveGatewayRef() gateway name = %q, want %q", ref.Name, "correct-gateway")
	}
	if ref.Namespace != "correct-ns" {
		t.Errorf("resolveGatewayRef() gateway namespace = %q, want %q", ref.Namespace, "correct-ns")
	}
	if model.Status.ResolvedTenantRef != "correct-tenant" {
		t.Errorf("resolveGatewayRef() ResolvedTenantRef = %q, want %q", model.Status.ResolvedTenantRef, "correct-tenant")
	}
}
