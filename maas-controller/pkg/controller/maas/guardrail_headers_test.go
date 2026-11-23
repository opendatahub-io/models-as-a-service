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

	"github.com/google/cel-go/cel"
	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

func TestGuardrailHeaders_ComposesAllAttachmentScopes(t *testing.T) {
	const tenantNamespace = "tenant-a"
	const modelNamespace = "models"

	objects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace, Labels: map[string]string{
			tenantreconcile.LabelManagedByAITenant: "true",
		}}},
		&maasv1alpha1.AITenant{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "ai-tenants"},
			Spec:       maasv1alpha1.AITenantSpec{Guardrails: testGuardrailAttachments("tenant-policy")},
			Status:     maasv1alpha1.AITenantStatus{TenantNamespace: tenantNamespace},
		},
		&maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      maasv1alpha1.MaasTenantConfigInstanceName,
				Namespace: tenantNamespace,
				Labels: map[string]string{
					tenantreconcile.LabelManagedByAITenant: "true",
				},
				Annotations: map[string]string{
					tenantreconcile.AnnotationAITenantName:      "tenant-a",
					tenantreconcile.AnnotationAITenantNamespace: "ai-tenants",
				},
			},
			Spec: maasv1alpha1.MaasTenantConfigSpec{Guardrails: testGuardrailAttachments("config-policy")},
		},
		&maasv1alpha1.MaaSSubscription{
			ObjectMeta: metav1.ObjectMeta{Name: "subscription-a", Namespace: tenantNamespace},
			Spec: maasv1alpha1.MaaSSubscriptionSpec{
				Guardrails: testGuardrailAttachments("subscription-policy"),
				ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{
					Name:       "model-a",
					Namespace:  modelNamespace,
					Unlimited:  true,
					Guardrails: testGuardrailAttachments("entry-policy"),
				}},
			},
		},
		&maasv1alpha1.MaaSModelRef{
			ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: modelNamespace},
			Spec:       maasv1alpha1.MaaSModelSpec{Guardrails: testGuardrailAttachments("model-policy")},
			Status:     maasv1alpha1.MaaSModelStatus{ResolvedTenantRef: "tenant-a"},
		},
		testGuardrailPolicy(tenantNamespace, "tenant-policy"),
		testGuardrailPolicy(tenantNamespace, "config-policy"),
		testGuardrailPolicy(tenantNamespace, "subscription-policy"),
		testGuardrailPolicy(tenantNamespace, "entry-policy"),
		testGuardrailPolicy(modelNamespace, "model-policy"),
	}

	r := newGuardrailHeaderTestReconciler(t, objects...)
	headers, err := r.guardrailHeaders(context.Background(), tenantNamespace)
	if err != nil {
		t.Fatalf("guardrailHeaders() error = %v", err)
	}
	if len(headers) != 5 {
		t.Fatalf("guardrailHeaders() returned %d headers, want 5: %#v", len(headers), headers)
	}
	want := map[string]string{}
	for _, policy := range []struct{ namespace, name string }{
		{tenantNamespace, "tenant-policy"},
		{tenantNamespace, "config-policy"},
		{tenantNamespace, "subscription-policy"},
		{tenantNamespace, "entry-policy"},
		{modelNamespace, "model-policy"},
	} {
		id := aiguardrail.CheckID(policy.namespace, policy.name, "pii")
		want[id] = aiguardrail.CheckHeaderName(policy.namespace, policy.name, "pii")
	}
	for _, header := range headers {
		if wantName, ok := want[header.ID]; !ok || header.Name != wantName {
			t.Fatalf("unexpected guardrail header: %#v", header)
		}
	}
}

func TestGuardrailHeaders_DeduplicatesChecksAcrossSelectionScopes(t *testing.T) {
	const tenantNamespace = "tenant-a"
	const modelNamespace = "models"

	attachments := testGuardrailAttachments("shared-policy")
	objects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace, Labels: map[string]string{
			tenantreconcile.LabelManagedByAITenant: "true",
		}}},
		&maasv1alpha1.AITenant{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "ai-tenants"},
			Spec:       maasv1alpha1.AITenantSpec{Guardrails: attachments},
			Status:     maasv1alpha1.AITenantStatus{TenantNamespace: tenantNamespace},
		},
		&maasv1alpha1.MaasTenantConfig{
			ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNamespace, Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
			}, Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName: "tenant-a", tenantreconcile.AnnotationAITenantNamespace: "ai-tenants",
			}},
			Spec: maasv1alpha1.MaasTenantConfigSpec{Guardrails: attachments},
		},
		&maasv1alpha1.MaaSSubscription{
			ObjectMeta: metav1.ObjectMeta{Name: "subscription-a", Namespace: tenantNamespace},
			Spec: maasv1alpha1.MaaSSubscriptionSpec{Guardrails: attachments, ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{
				Name: "model-a", Namespace: modelNamespace, Unlimited: true, Guardrails: attachments,
			}}},
		},
		&maasv1alpha1.MaaSModelRef{
			ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: modelNamespace},
			Status:     maasv1alpha1.MaaSModelStatus{ResolvedTenantRef: "tenant-a"},
		},
		testGuardrailPolicy(tenantNamespace, "shared-policy"),
	}

	headers, err := newGuardrailHeaderTestReconciler(t, objects...).guardrailHeaders(context.Background(), tenantNamespace)
	if err != nil {
		t.Fatalf("guardrailHeaders() error = %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("guardrailHeaders() returned %#v, want one deduplicated header", headers)
	}
}

func TestGuardrailHeaders_ReturnsNoPartialResultOnResolutionFailure(t *testing.T) {
	const tenantNamespace = "tenant-a"
	const modelNamespace = "models"
	objects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace, Labels: map[string]string{
			tenantreconcile.LabelManagedByAITenant: "true",
		}}},
		&maasv1alpha1.AITenant{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-a", Namespace: "ai-tenants"},
			Status:     maasv1alpha1.AITenantStatus{TenantNamespace: tenantNamespace},
		},
		&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
			Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: tenantNamespace, Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
			}, Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName: "tenant-a", tenantreconcile.AnnotationAITenantNamespace: "ai-tenants",
			},
		}},
		&maasv1alpha1.MaaSSubscription{
			ObjectMeta: metav1.ObjectMeta{Name: "subscription-a", Namespace: tenantNamespace},
			Spec: maasv1alpha1.MaaSSubscriptionSpec{ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
				{Name: "model-a", Namespace: modelNamespace, Unlimited: true},
				{Name: "model-b", Namespace: modelNamespace, Unlimited: true, Guardrails: testGuardrailAttachments("missing-policy")},
			}},
		},
		&maasv1alpha1.MaaSModelRef{
			ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: modelNamespace},
			Spec:       maasv1alpha1.MaaSModelSpec{Guardrails: testGuardrailAttachments("valid-policy")},
			Status:     maasv1alpha1.MaaSModelStatus{ResolvedTenantRef: "tenant-a"},
		},
		&maasv1alpha1.MaaSModelRef{
			ObjectMeta: metav1.ObjectMeta{Name: "model-b", Namespace: modelNamespace},
			Status:     maasv1alpha1.MaaSModelStatus{ResolvedTenantRef: "tenant-a"},
		},
		testGuardrailPolicy(modelNamespace, "valid-policy"),
	}

	headers, err := newGuardrailHeaderTestReconciler(t, objects...).guardrailHeaders(context.Background(), tenantNamespace)
	if err == nil {
		t.Fatal("guardrailHeaders() error = nil, want resolution failure")
	}
	if headers != nil {
		t.Fatalf("guardrailHeaders() returned partial headers %#v", headers)
	}
}

func TestGuardrailHeaders_MissingTenantConfig(t *testing.T) {
	const tenantNamespace = "tenant-a"

	tests := []struct {
		name         string
		objects      []runtime.Object
		wantErr      bool
		wantNotFound bool
	}{
		{
			name: "legacy namespace returns no catalog",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace}},
			},
		},
		{
			name: "AITenant managed namespace returns an error",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: tenantNamespace,
					Labels: map[string]string{
						tenantreconcile.LabelManagedByAITenant: "true",
					},
				}},
			},
			wantErr: true,
		},
		{
			name:         "missing namespace returns an error",
			wantErr:      true,
			wantNotFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers, err := newGuardrailHeaderTestReconciler(t, tt.objects...).guardrailHeaders(context.Background(), tenantNamespace)
			if tt.wantErr != (err != nil) {
				t.Fatalf("guardrailHeaders() error = %v, want error: %v", err, tt.wantErr)
			}
			if headers != nil {
				t.Fatalf("guardrailHeaders() returned headers %#v, want nil", headers)
			}
			if tt.wantNotFound && !apierrors.IsNotFound(err) {
				t.Fatalf("guardrailHeaders() error = %v, want namespace NotFound", err)
			}
			if tt.wantNotFound && !strings.Contains(err.Error(), tenantNamespace) {
				t.Fatalf("guardrailHeaders() error = %q, want namespace %q", err, tenantNamespace)
			}
		})
	}
}

func TestGuardrailHeaders_TenantOwnership(t *testing.T) {
	const defaultNamespace = "models-as-a-service"

	tests := []struct {
		name        string
		tenantNS    string
		objects     []runtime.Object
		wantErr     string
		wantNil     bool
		wantHeaders int
	}{
		{
			name:     "discovery namespace config without managed label skips catalog resolution",
			tenantNS: "legacy-tenant",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy-tenant", Labels: map[string]string{
					tenantreconcile.LabelAIGatewayTenant: "legacy-tenant",
				}}},
				&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "legacy-tenant",
				}},
			},
			wantNil: true,
		},
		{
			name:     "managed tenant config requires ownership annotations",
			tenantNS: "managed-tenant",
			objects: []runtime.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "managed-tenant", Labels: map[string]string{
					tenantreconcile.LabelManagedByAITenant: "true",
				}}},
				&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "managed-tenant", Labels: map[string]string{
						tenantreconcile.LabelManagedByAITenant: "true",
					},
				}},
			},
			wantErr: "missing owning AITenant annotations",
		},
		{
			name:     "default tenant config uses default AITenant",
			tenantNS: defaultNamespace,
			objects: []runtime.Object{
				&maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{
					Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: defaultNamespace, Labels: map[string]string{
						tenantreconcile.LabelManagedByAITenant: "true",
					},
				}},
				&maasv1alpha1.AITenant{
					ObjectMeta: metav1.ObjectMeta{Name: tenantreconcile.DefaultAITenantName, Namespace: tenantreconcile.DefaultAITenantNamespace},
					Status:     maasv1alpha1.AITenantStatus{TenantNamespace: defaultNamespace},
				},
			},
			wantHeaders: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newGuardrailHeaderTestReconciler(t, tt.objects...)
			r.TenantNamespace = defaultNamespace
			headers, err := r.guardrailHeaders(context.Background(), tt.tenantNS)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("guardrailHeaders() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("guardrailHeaders() error = %v", err)
			}
			if tt.wantNil && headers != nil {
				t.Fatalf("guardrailHeaders() = %#v, want nil", headers)
			}
			if !tt.wantNil {
				if headers == nil {
					t.Fatal("guardrailHeaders() = nil, want resolved catalog")
				}
				if len(headers) != tt.wantHeaders {
					t.Fatalf("guardrailHeaders() returned %d headers, want %d", len(headers), tt.wantHeaders)
				}
			}
		})
	}
}

func TestGatewayAuthPolicySpec_AddsSelectedGuardrailHeaders(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{}
	checkID := aiguardrail.CheckID("tenant-a", "privacy", "pii")
	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "gateway-ns", "gateway", []guardrailHeader{{
		ID:   checkID,
		Name: aiguardrail.CheckHeaderName("tenant-a", "privacy", "pii"),
	}})

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	headers := nestedMapRequired(t, obj, "spec", "defaults", "rules", "response", "success", "headers")
	headerName := aiguardrail.CheckHeaderName("tenant-a", "privacy", "pii")
	expression := nestedStringRequired(t, &unstructured.Unstructured{Object: map[string]any{"header": headers[headerName]}}, "header", "plain", "expression")
	if !strings.Contains(expression, `selectedGuardrailChecks`) || !strings.Contains(expression, checkID) {
		t.Fatalf("guardrail expression does not read the selected check map: %q", expression)
	}
}

func TestGatewayAuthPolicySpec_ValidatesEmptyGuardrailCatalog(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{}
	spec := r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "gateway-ns", "gateway", []guardrailHeader{})
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}

	rego := nestedStringRequired(t, obj, "spec", "defaults", "rules", "authorization", "guardrail-checks-known", "opa", "rego")
	if !strings.Contains(rego, "id in []") {
		t.Fatalf("empty guardrail catalog must reject selected checks, got rego:\n%s", rego)
	}
}

func TestSelectedGuardrailHeader_UsesSelectedCheckMap(t *testing.T) {
	checkID := aiguardrail.CheckID("tenant-a", "privacy", "pii")
	header := selectedGuardrailHeader(checkID)
	plain, ok := header["plain"].(map[string]any)
	if !ok {
		t.Fatal("selected guardrail header is missing the plain expression")
	}
	expression, ok := plain["expression"].(string)
	if !ok {
		t.Fatal("selected guardrail header has no CEL expression")
	}
	env, err := cel.NewEnv(cel.Variable("auth", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("Compile: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		auth map[string]any
		want string
	}{
		{name: "selected", auth: map[string]any{"metadata": map[string]any{"subscription-info": map[string]any{"selectedGuardrailChecks": map[string]bool{checkID: true}}}}, want: "true"},
		{name: "not selected", auth: map[string]any{"metadata": map[string]any{"subscription-info": map[string]any{"selectedGuardrailChecks": map[string]bool{}}}}, want: "false"},
		{name: "selection missing", auth: map[string]any{"metadata": map[string]any{"subscription-info": map[string]any{}}}, want: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := program.Eval(map[string]any{"auth": tc.auth})
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got := out.Value(); got != tc.want {
				t.Fatalf("value = %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectedGuardrailChecksRego_EnablesEveryKeyword(t *testing.T) {
	rego := selectedGuardrailChecksRego([]guardrailHeader{{
		ID: aiguardrail.CheckID("tenant-a", "privacy", "pii"),
	}})
	if !strings.HasPrefix(rego, "import future.keywords.every\n\nallow {") {
		t.Fatalf("rego must enable the every keyword for Rego v0: %q", rego)
	}
}

func TestAIGatewayResolutionChangedPredicate_UpdatesOnlyForResolverInputs(t *testing.T) {
	newGuardrail := func() *aigatewayv1alpha1.AIGuardrail {
		return &aigatewayv1alpha1.AIGuardrail{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: aigatewayv1alpha1.AIGuardrailStatus{
				ObservedGeneration: 1,
				BindingRevision:    "revision-a",
				Conditions: []metav1.Condition{
					{Type: aiguardrail.ConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1},
					{Type: "ProviderReady", Status: metav1.ConditionTrue, ObservedGeneration: 1},
				},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*aigatewayv1alpha1.AIGuardrail)
		want   bool
	}{
		{
			name: "spec generation changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Generation = 2
			},
			want: true,
		},
		{
			name: "binding revision changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Status.BindingRevision = "revision-b"
			},
			want: true,
		},
		{
			name: "accepted status changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			want: true,
		},
		{
			name: "accepted observed generation changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Status.Conditions[0].ObservedGeneration = 0
			},
			want: true,
		},
		{
			name: "top level observed generation changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Status.ObservedGeneration = 2
			},
			want: false,
		},
		{
			name: "unrelated condition changed",
			mutate: func(policy *aigatewayv1alpha1.AIGuardrail) {
				policy.Status.Conditions[1].Status = metav1.ConditionFalse
			},
			want: false,
		},
	}

	predicate := aiGatewayResolutionChangedPredicate{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldPolicy := newGuardrail()
			newPolicy := oldPolicy.DeepCopy()
			tt.mutate(newPolicy)
			got := predicate.Update(event.UpdateEvent{ObjectOld: oldPolicy, ObjectNew: newPolicy})
			if got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapObjectNamespaceToMaaSAuthPolicies_UsesObjectNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := maasv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	policy := &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: "tenant-a"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(policy).Build()
	r := &MaaSAuthPolicyReconciler{Client: client, TenantNamespace: "tenant-a"}

	requests := r.mapObjectNamespaceToMaaSAuthPolicies(context.Background(), &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: "subscription-a", Namespace: "tenant-a"},
	})
	if len(requests) != 1 || requests[0].Namespace != "tenant-a" || requests[0].Name != "policy-a" {
		t.Fatalf("requests = %#v, want tenant-a/policy-a", requests)
	}
}

func testGuardrailAttachments(policy string) []maasv1alpha1.GuardrailAttachment {
	return []maasv1alpha1.GuardrailAttachment{{Ref: maasv1alpha1.GuardrailRef{Name: policy}}}
}

func newGuardrailHeaderTestReconciler(t *testing.T, objects ...runtime.Object) *MaaSAuthPolicyReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := maasv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := aigatewayv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &MaaSAuthPolicyReconciler{
		Client:          fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(),
		TenantNamespace: tenantreconcile.DefaultAITenantName,
	}
}

func testGuardrailPolicy(namespace, name string) *aigatewayv1alpha1.AIGuardrail {
	return &aigatewayv1alpha1.AIGuardrail{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 1},
		Spec: aigatewayv1alpha1.AIGuardrailSpec{Checks: []aigatewayv1alpha1.AIGuardrailCheck{{
			Name: "pii", Phases: []aigatewayv1alpha1.GuardrailPhase{aigatewayv1alpha1.GuardrailPhaseInput},
		}}},
		Status: aigatewayv1alpha1.AIGuardrailStatus{
			BindingRevision: "ready",
			Conditions: []metav1.Condition{{
				Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 1,
			}},
		},
	}
}
