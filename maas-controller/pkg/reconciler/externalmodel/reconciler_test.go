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

package externalmodel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

var testScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(gatewayapiv1.Install(testScheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(testScheme))
}

// newTestExternalModel creates a minimal ExternalModel for reconciler tests.
func newTestExternalModel(name, ns, endpoint string, annotations map[string]string) *maasv1alpha1.ExternalModel {
	em := &maasv1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: annotations,
		},
		Spec: maasv1alpha1.ExternalModelSpec{
			Provider:    "openai",
			Endpoint:    endpoint,
			TargetModel: name,
		},
	}
	// TypeMeta is needed for setUnstructuredOwner to build a correct OwnerReference.
	em.APIVersion = maasv1alpha1.GroupVersion.String()
	em.Kind = "ExternalModel"
	return em
}

// TestManagedAnnotation_Service verifies that an existing Service with
// opendatahub.io/managed=false is not updated by the ExternalModel reconciler.
func TestManagedAnnotation_Service(t *testing.T) {
	const (
		name                 = "gpt-4o"
		ns                   = "llm"
		endpoint             = "api.openai.com"
		sentinelExternalName = "sentinel.example.com"
	)

	tests := []struct {
		testName        string
		annotations     map[string]string
		wantSpecChanged bool
	}{
		{
			testName:        "annotation absent: controller updates",
			annotations:     nil,
			wantSpecChanged: true,
		},
		{
			testName:        "opendatahub.io/managed=false: controller skips update",
			annotations:     map[string]string{tenantreconcile.AnnotationManaged: "false"},
			wantSpecChanged: false,
		},
		{
			testName:        "opendatahub.io/managed=true: controller updates",
			annotations:     map[string]string{tenantreconcile.AnnotationManaged: "true"},
			wantSpecChanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:        name,
					Namespace:   ns,
					Annotations: tc.annotations,
				},
				Spec: corev1.ServiceSpec{
					Type:         corev1.ServiceTypeExternalName,
					ExternalName: sentinelExternalName,
				},
			}

			em := newTestExternalModel(name, ns, endpoint, nil)
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(em, existingSvc).Build()
			r := &Reconciler{Client: c, Scheme: testScheme, Log: ctrl.Log}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			require.NoError(t, err)

			got := &corev1.Service{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got))

			if tc.wantSpecChanged {
				assert.Equal(t, endpoint, got.Spec.ExternalName, "expected Service ExternalName to be updated to endpoint")
			} else {
				assert.Equal(t, sentinelExternalName, got.Spec.ExternalName, "expected Service ExternalName to be unchanged (managed=false opt-out)")
			}
		})
	}
}

// TestManagedAnnotation_ServiceEntry verifies that an existing ServiceEntry with
// opendatahub.io/managed=false is not updated by the ExternalModel reconciler.
func TestManagedAnnotation_ServiceEntry(t *testing.T) {
	const (
		name             = "gpt-4o"
		ns               = "llm"
		endpoint         = "api.openai.com"
		sentinelEndpoint = "sentinel.example.com"
	)

	tests := []struct {
		testName        string
		annotations     map[string]string
		wantSpecChanged bool
	}{
		{
			testName:        "annotation absent: controller updates",
			annotations:     nil,
			wantSpecChanged: true,
		},
		{
			testName:        "opendatahub.io/managed=false: controller skips update",
			annotations:     map[string]string{tenantreconcile.AnnotationManaged: "false"},
			wantSpecChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			existingSE := &unstructured.Unstructured{}
			existingSE.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry"})
			existingSE.SetName(name)
			existingSE.SetNamespace(ns)
			existingSE.SetAnnotations(tc.annotations)
			_ = unstructured.SetNestedStringSlice(existingSE.Object, []string{sentinelEndpoint}, "spec", "hosts")

			em := newTestExternalModel(name, ns, endpoint, nil)
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(em, existingSE).Build()
			r := &Reconciler{Client: c, Scheme: testScheme, Log: ctrl.Log}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			require.NoError(t, err)

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry"})
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got))

			hosts, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "hosts")
			if tc.wantSpecChanged {
				assert.Equal(t, []string{endpoint}, hosts, "expected ServiceEntry hosts to be updated to endpoint")
			} else {
				require.Len(t, hosts, 1)
				assert.Equal(t, sentinelEndpoint, hosts[0], "expected ServiceEntry hosts to be unchanged (managed=false opt-out)")
			}
		})
	}
}

// TestManagedAnnotation_HTTPRoute verifies that an existing HTTPRoute with
// opendatahub.io/managed=false is not updated by the ExternalModel reconciler.
func TestManagedAnnotation_HTTPRoute(t *testing.T) {
	const (
		name     = "gpt-4o"
		ns       = "llm"
		endpoint = "api.openai.com"
	)

	sentinelParentName := gatewayapiv1.ObjectName("sentinel-gateway")

	tests := []struct {
		testName        string
		annotations     map[string]string
		wantSpecChanged bool
	}{
		{
			testName:        "annotation absent: controller updates",
			annotations:     nil,
			wantSpecChanged: true,
		},
		{
			testName:        "opendatahub.io/managed=false: controller skips update",
			annotations:     map[string]string{tenantreconcile.AnnotationManaged: "false"},
			wantSpecChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			existingHR := &gatewayapiv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:        name,
					Namespace:   ns,
					Annotations: tc.annotations,
				},
				Spec: gatewayapiv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayapiv1.CommonRouteSpec{
						ParentRefs: []gatewayapiv1.ParentReference{
							{Name: sentinelParentName},
						},
					},
				},
			}

			em := newTestExternalModel(name, ns, endpoint, nil)
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(em, existingHR).Build()
			r := &Reconciler{Client: c, Scheme: testScheme, Log: ctrl.Log, GatewayName: "maas-default-gateway", GatewayNamespace: "openshift-ingress"}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			require.NoError(t, err)

			got := &gatewayapiv1.HTTPRoute{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got))

			if tc.wantSpecChanged {
				require.Len(t, got.Spec.ParentRefs, 1)
				assert.Equal(t, gatewayapiv1.ObjectName("maas-default-gateway"), got.Spec.ParentRefs[0].Name,
					"expected HTTPRoute parent to be updated to configured gateway")
			} else {
				require.Len(t, got.Spec.ParentRefs, 1)
				assert.Equal(t, sentinelParentName, got.Spec.ParentRefs[0].Name,
					"expected HTTPRoute parent to be unchanged (managed=false opt-out)")
			}
		})
	}
}

// TestManagedAnnotation_DestinationRule_DeletePath verifies that an existing DestinationRule with
// opendatahub.io/managed=false is NOT deleted when the ExternalModel switches to TLS=false.
// (deleteIfExists is called to remove the stale DestinationRule when TLS is disabled.)
func TestManagedAnnotation_DestinationRule_DeletePath(t *testing.T) {
	const (
		name     = "gpt-4o"
		ns       = "llm"
		endpoint = "vllm.internal"
	)

	tests := []struct {
		testName    string
		annotations map[string]string
		wantDeleted bool
	}{
		{
			testName:    "annotation absent: controller deletes stale DestinationRule",
			annotations: nil,
			wantDeleted: true,
		},
		{
			testName:    "opendatahub.io/managed=true: controller deletes stale DestinationRule",
			annotations: map[string]string{tenantreconcile.AnnotationManaged: "true"},
			wantDeleted: true,
		},
		{
			testName:    "opendatahub.io/managed=false: controller must not delete opted-out DestinationRule",
			annotations: map[string]string{tenantreconcile.AnnotationManaged: "false"},
			wantDeleted: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			// Pre-populate a stale DestinationRule (left over from when TLS was enabled).
			existingDR := &unstructured.Unstructured{}
			existingDR.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"})
			existingDR.SetName(name)
			existingDR.SetNamespace(ns)
			existingDR.SetAnnotations(tc.annotations)

			// ExternalModel has tls=false annotation so the reconciler calls deleteIfExists on the DR.
			em := newTestExternalModel(name, ns, endpoint, map[string]string{annotationTLS: "false"})
			c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(em, existingDR).Build()
			r := &Reconciler{Client: c, Scheme: testScheme, Log: ctrl.Log}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
			require.NoError(t, err)

			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"})
			getErr := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, got)

			if tc.wantDeleted {
				assert.True(t, apierrors.IsNotFound(getErr),
					"expected DestinationRule to be deleted, but it still exists (err: %v)", getErr)
			} else {
				assert.NoError(t, getErr,
					"expected DestinationRule to survive (managed=false opt-out), but got error: %v", getErr)
			}
		})
	}
}

// TestIsManaged verifies the isManaged helper function covers all edge cases.
func TestIsManaged(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "nil annotations", annotations: nil, want: true},
		{name: "empty annotations", annotations: map[string]string{}, want: true},
		{name: "other annotations only", annotations: map[string]string{"other.io/key": "value"}, want: true},
		{name: "managed=false opts out", annotations: map[string]string{tenantreconcile.AnnotationManaged: "false"}, want: false},
		{name: "managed=true is managed", annotations: map[string]string{tenantreconcile.AnnotationManaged: "true"}, want: true},
		{name: "managed=1 is managed (only 'false' opts out)", annotations: map[string]string{tenantreconcile.AnnotationManaged: "1"}, want: true},
		{name: "managed=FALSE (uppercase) is managed", annotations: map[string]string{tenantreconcile.AnnotationManaged: "FALSE"}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			assert.Equal(t, tc.want, isManaged(obj))
		})
	}
}
