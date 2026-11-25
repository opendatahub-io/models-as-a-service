package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCleanupLegacyMaaSAPIPodMonitor(t *testing.T) {
	const appNs = "redhat-ai-gateway-infra"
	configUID := types.UID("config-uid")
	controller := true
	podMonitor := func(ownerUID types.UID, manager string) *unstructured.Unstructured {
		pm := &unstructured.Unstructured{}
		pm.SetGroupVersionKind(GVKPodMonitor)
		pm.SetNamespace(appNs)
		pm.SetName("maas-api-metrics")
		pm.SetUID("podmonitor-uid")
		if ownerUID != "" {
			pm.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: "maas.opendatahub.io/v1alpha1", Kind: "Config", Name: "owner", UID: ownerUID, Controller: &controller,
			}})
		}
		pm.SetManagedFields([]metav1.ManagedFieldsEntry{{
			Manager: manager, Operation: metav1.ManagedFieldsOperationApply, APIVersion: GVKPodMonitor.GroupVersion().String(),
			FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{}}`)},
		}})
		return pm
	}
	optedOut := podMonitor(configUID, ssaFieldOwner)
	optedOut.SetAnnotations(map[string]string{AnnotationManaged: "false"})
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: GVKPodMonitor.Group, Resource: "podmonitors"}, "maas-api-metrics", errors.New("denied"))

	tests := []struct {
		name     string
		existing *unstructured.Unstructured
		getErr   error
		wantKept bool
		wantErr  bool
	}{
		{name: "owned by Config", existing: podMonitor(configUID, "kubectl")},
		{name: "owned by another controller", existing: podMonitor("other-uid", ssaFieldOwner), wantKept: true},
		{name: "annotated managed=false", existing: optedOut, wantKept: true},
		{name: "absent"},
		{name: "PodMonitor CRD not installed", getErr: &meta.NoKindMatchError{GroupKind: GVKPodMonitor.GroupKind()}},
		{name: "other read error", getErr: forbidden, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			scheme.AddKnownTypeWithName(GVKPodMonitor, &unstructured.Unstructured{})
			mapper := meta.NewDefaultRESTMapper(nil)
			mapper.Add(GVKPodMonitor, meta.RESTScopeNamespace)
			builder := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithReturnManagedFields()
			if tt.existing != nil {
				builder = builder.WithObjects(tt.existing)
			}
			if tt.getErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return tt.getErr
					},
				})
			}
			c := builder.Build()

			err := cleanupLegacyMaaSAPIPodMonitor(context.Background(), c, appNs, configUID, logr.Discard())
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.existing != nil {
				getErr := c.Get(context.Background(), client.ObjectKeyFromObject(tt.existing), podMonitor("", ""))
				assert.Equal(t, tt.wantKept, getErr == nil, "get after cleanup: %v", getErr)
			}
		})
	}
}

// Only overlays that render the replacement ServiceMonitor look for the PodMonitor.
func TestRendersMaaSAPIServiceMonitor(t *testing.T) {
	assert.True(t, rendersMaaSAPIServiceMonitor(renderPlatformOverlayResources(t, "odh", "maas-infra")))
	assert.False(t, rendersMaaSAPIServiceMonitor(renderPlatformOverlayResources(t, "xks", "maas-infra")))
}
