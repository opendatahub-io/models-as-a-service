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
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayapiv1.Install(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
}

var scheme = runtime.NewScheme()

// nsRestScope implements apimeta.RESTScope for namespace-scoped resources.
type nsRestScope struct{}

func (nsRestScope) Name() apimeta.RESTScopeName { return apimeta.RESTScopeNameNamespace }

// testRESTMapper builds a REST mapper covering all GVKs exercised across
// controller tests, including Kuadrant types that are not registered in the scheme.
func testRESTMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper(nil)
	ns := nsRestScope{}
	m.Add(schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSModel"}, ns)
	m.Add(schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSAuthPolicy"}, ns)
	m.Add(schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription"}, ns)
	m.Add(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"}, ns)
	m.Add(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicyList"}, ns)
	return m
}

// newHTTPRoute creates a plain HTTPRoute (no labels or parent refs).
// Used by ExternalModel tests where KServe labels are not expected.
func newHTTPRoute(name, ns string) *gatewayapiv1.HTTPRoute {
	return &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
}

// newMaaSSubscription creates a MaaSSubscription with a single owner group and a single
// model ref with the given token rate limit. The model namespace defaults to the
// subscription's namespace (ns).
func newMaaSSubscription(name, ns, group, modelName string, limit int64) *maasv1alpha1.MaaSSubscription {
	return &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner: maasv1alpha1.OwnerSpec{
				Groups: []maasv1alpha1.GroupReference{{Name: group}},
			},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{
				{Name: modelName, Namespace: ns, TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: limit, Window: "1m"}}},
			},
		},
	}
}

// newMaaSAuthPolicy creates a MaaSAuthPolicy with a single subject group and the given model refs.
func newMaaSAuthPolicy(name, ns, group string, modelRefs ...maasv1alpha1.ModelRef) *maasv1alpha1.MaaSAuthPolicy {
	return &maasv1alpha1.MaaSAuthPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSAuthPolicySpec{
			ModelRefs: modelRefs,
			Subjects:  maasv1alpha1.SubjectSpec{Groups: []maasv1alpha1.GroupReference{{Name: group}}},
		},
	}
}

func TestGetBackendHandler_UnknownKind_ReturnsNil(t *testing.T) {
	r := &MaaSModelRefReconciler{}
	if got := GetBackendHandler("unknown", r); got != nil {
		t.Errorf("GetBackendHandler(%q) = %v, want nil", "unknown", got)
	}
}

func TestGetBackendHandler_Llmisvc_ReturnsHandler(t *testing.T) {
	r := &MaaSModelRefReconciler{}
	got := GetBackendHandler("llmisvc", r)
	if got == nil {
		t.Fatal("GetBackendHandler(\"llmisvc\") = nil, want non-nil")
	}
	if _, ok := got.(*llmisvcHandler); !ok {
		t.Errorf("GetBackendHandler(\"llmisvc\") = %T, want *llmisvcHandler", got)
	}
}

func TestGetBackendHandler_LLMInferenceService_ReturnsHandler(t *testing.T) {
	// CRD enum value is LLMInferenceService; ensure it resolves to the same handler as llmisvc.
	r := &MaaSModelRefReconciler{}
	got := GetBackendHandler("LLMInferenceService", r)
	if got == nil {
		t.Fatal("GetBackendHandler(\"LLMInferenceService\") = nil, want non-nil")
	}
	if _, ok := got.(*llmisvcHandler); !ok {
		t.Errorf("GetBackendHandler(\"LLMInferenceService\") = %T, want *llmisvcHandler", got)
	}
}

func TestGetBackendHandler_ExternalModel_ReturnsHandler(t *testing.T) {
	r := &MaaSModelRefReconciler{}
	got := GetBackendHandler("ExternalModel", r)
	if got == nil {
		t.Fatal("GetBackendHandler(\"ExternalModel\") = nil, want non-nil")
	}
	if _, ok := got.(*externalModelHandler); !ok {
		t.Errorf("GetBackendHandler(\"ExternalModel\") = %T, want *externalModelHandler", got)
	}
}

func TestGetRouteResolver_UnknownKind_ReturnsNil(t *testing.T) {
	if got := GetRouteResolver("unknown"); got != nil {
		t.Errorf("GetRouteResolver(%q) = %v, want nil", "unknown", got)
	}
}

func TestGetRouteResolver_Llmisvc_ReturnsResolver(t *testing.T) {
	got := GetRouteResolver("llmisvc")
	if got == nil {
		t.Fatal("GetRouteResolver(\"llmisvc\") = nil, want non-nil")
	}
	if _, ok := got.(*llmisvcRouteResolver); !ok {
		t.Errorf("GetRouteResolver(\"llmisvc\") = %T, want *llmisvcRouteResolver", got)
	}
}

func TestGetRouteResolver_LLMInferenceService_ReturnsResolver(t *testing.T) {
	got := GetRouteResolver("LLMInferenceService")
	if got == nil {
		t.Fatal("GetRouteResolver(\"LLMInferenceService\") = nil, want non-nil")
	}
	if _, ok := got.(*llmisvcRouteResolver); !ok {
		t.Errorf("GetRouteResolver(\"LLMInferenceService\") = %T, want *llmisvcRouteResolver", got)
	}
}

func TestGetRouteResolver_ExternalModel_ReturnsResolver(t *testing.T) {
	got := GetRouteResolver("ExternalModel")
	if got == nil {
		t.Fatal("GetRouteResolver(\"ExternalModel\") = nil, want non-nil")
	}
	if _, ok := got.(*externalModelRouteResolver); !ok {
		t.Errorf("GetRouteResolver(\"ExternalModel\") = %T, want *externalModelRouteResolver", got)
	}
}

func TestErrModelNotFound(t *testing.T) {
	// Controller uses fmt.Errorf("%w: %s", ErrModelNotFound, modelName)
	err := fmt.Errorf("%w: %s", ErrModelNotFound, "test-model")
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("errors.Is(wrapped ErrModelNotFound, ErrModelNotFound) = false")
	}
}

func TestFindHTTPRouteForModel_NotFound(t *testing.T) {
	ctx := context.Background()
	b := fake.NewClientBuilder().WithScheme(scheme)
	// No MaaSModelRefs
	c := b.Build()
	_, _, err := findHTTPRouteForModel(ctx, c, "default", "nonexistent")
	if err == nil {
		t.Fatal("findHTTPRouteForModel: expected error, got nil")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("findHTTPRouteForModel: expected ErrModelNotFound, got %v", err)
	}
}

func TestFindHTTPRouteForModel_UnknownKind(t *testing.T) {
	ctx := context.Background()
	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "UnknownKind", Name: "x"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model).Build()
	_, _, err := findHTTPRouteForModel(ctx, c, "default", "m")
	if err == nil {
		t.Fatal("findHTTPRouteForModel: expected error for unknown kind, got nil")
	}
	if err.Error() != "unknown model kind: UnknownKind" {
		t.Errorf("findHTTPRouteForModel: got %v", err)
	}
}

func TestFindHTTPRouteForModel_ExternalModel_Success(t *testing.T) {
	ctx := context.Background()
	model := &maasv1alpha1.MaaSModelRef{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: maasv1alpha1.MaaSModelSpec{
			ModelRef: maasv1alpha1.ModelReference{Kind: "ExternalModel", Name: "foo"},
		},
	}
	route := &gatewayapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "maas-model-foo", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model, route).Build()
	routeName, routeNS, err := findHTTPRouteForModel(ctx, c, "default", "foo")
	if err != nil {
		t.Fatalf("findHTTPRouteForModel: %v", err)
	}
	if routeName != "maas-model-foo" || routeNS != "default" {
		t.Errorf("findHTTPRouteForModel: got (%q, %q), want (\"maas-model-foo\", \"default\")", routeName, routeNS)
	}
}

func TestLlmisvcHandler_CleanupOnDelete(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestReconciler()
	h := &llmisvcHandler{r: r}

	model := newMaaSModelRef("test", "default", "LLMInferenceService", "test-llmisvc")

	// CleanupOnDelete should always succeed and do nothing for llmisvc
	// (HTTPRoutes are owned by KServe, not by the MaaSModelRef controller)
	err := h.CleanupOnDelete(ctx, logr.Discard(), model)
	if err != nil {
		t.Errorf("CleanupOnDelete() error = %v, want nil", err)
	}
}

func TestLlmisvcRouteResolver_HTTPRouteForModel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		model         *maasv1alpha1.MaaSModelRef
		routes        []*gatewayapiv1.HTTPRoute
		wantRouteName string
		wantRouteNS   string
		wantErr       bool
	}{
		{
			name:  "HTTPRoute found",
			model: newMaaSModelRef("model1", "default", "LLMInferenceService", "test-llmisvc"),
			routes: []*gatewayapiv1.HTTPRoute{
				newLLMISvcRoute("test-llmisvc", "default"),
			},
			wantRouteName: "test-llmisvc-route",
			wantRouteNS:   "default",
			wantErr:       false,
		},
		{
			name:          "HTTPRoute not found",
			model:         newMaaSModelRef("model1", "default", "LLMInferenceService", "test-llmisvc"),
			routes:        nil,
			wantRouteName: "",
			wantRouteNS:   "",
			wantErr:       true,
		},
		{
			name:  "HTTPRoute with different labels not matched",
			model: newMaaSModelRef("model1", "default", "LLMInferenceService", "test-llmisvc"),
			routes: []*gatewayapiv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-route",
						Namespace: "default",
						Labels: map[string]string{
							"app.kubernetes.io/name": "other-llmisvc",
						},
					},
				},
			},
			wantRouteName: "",
			wantRouteNS:   "",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []client.Object
			if tt.model != nil {
				objects = append(objects, tt.model)
			}
			for _, route := range tt.routes {
				objects = append(objects, route)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()

			resolver := llmisvcRouteResolver{}
			routeName, routeNS, err := resolver.HTTPRouteForModel(ctx, c, tt.model)

			if (err != nil) != tt.wantErr {
				t.Errorf("HTTPRouteForModel() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if routeName != tt.wantRouteName {
				t.Errorf("HTTPRouteForModel() routeName = %v, want %v", routeName, tt.wantRouteName)
			}

			if routeNS != tt.wantRouteNS {
				t.Errorf("HTTPRouteForModel() routeNS = %v, want %v", routeNS, tt.wantRouteNS)
			}
		})
	}
}
