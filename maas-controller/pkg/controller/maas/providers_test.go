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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

func TestGetBackendHandler_UnknownKind_ReturnsNil(t *testing.T) {
	r := &MaaSModelReconciler{}
	if got := GetBackendHandler("unknown", r); got != nil {
		t.Errorf("GetBackendHandler(%q) = %v, want nil", "unknown", got)
	}
}

func TestGetBackendHandler_Llmisvc_ReturnsHandler(t *testing.T) {
	r := &MaaSModelReconciler{}
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
	r := &MaaSModelReconciler{}
	got := GetBackendHandler("LLMInferenceService", r)
	if got == nil {
		t.Fatal("GetBackendHandler(\"LLMInferenceService\") = nil, want non-nil")
	}
	if _, ok := got.(*llmisvcHandler); !ok {
		t.Errorf("GetBackendHandler(\"LLMInferenceService\") = %T, want *llmisvcHandler", got)
	}
}

func TestGetBackendHandler_ExternalModel_ReturnsHandler(t *testing.T) {
	r := &MaaSModelReconciler{}
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
	// No MaaSModels
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
	model := &maasv1alpha1.MaaSModel{
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
	model := &maasv1alpha1.MaaSModel{
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
