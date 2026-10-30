package config //nolint:testpackage // Tests verify optional informer registration and cache sync wiring.

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestSetupGuardrailInformer_OnlyRegistersWhenAPIServed(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "aigateway.opendatahub.io", Version: "v1alpha1", Resource: "aiguardrails"}
	for _, tc := range []struct {
		name      string
		resources []metav1.APIResource
		err       error
		wantWatch bool
	}{
		{name: "resource available", resources: []metav1.APIResource{{Name: "aiguardrails"}}, wantWatch: true},
		{name: "group available without resource", resources: []metav1.APIResource{{Name: "otherresources"}}},
		{name: "API absent", err: apierrors.NewNotFound(gvr.GroupResource(), "")},
		{name: "discovery forbidden", err: apierrors.NewForbidden(gvr.GroupResource(), "", errors.New("denied"))},
		{name: "discovery unavailable", err: apierrors.NewServiceUnavailable("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			discoveryClient := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
			discoveryClient.Resources = []*metav1.APIResourceList{{GroupVersion: gvr.GroupVersion().String(), APIResources: tc.resources}}
			if tc.err != nil {
				discoveryClient.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.err
				})
			}
			factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), 0)
			cluster := &ClusterConfig{log: logr.Discard()}
			err := cluster.setupGuardrailInformer(context.Background(), discoveryClient, factory)
			if tc.err != nil && !apierrors.IsNotFound(tc.err) {
				require.ErrorIs(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
			if tc.wantWatch {
				require.NotNil(t, cluster.AIGuardrailGetter)
				require.Len(t, cluster.informersSynced, 1)
			} else {
				require.Nil(t, cluster.AIGuardrailGetter)
				require.Empty(t, cluster.informersSynced)
			}
		})
	}
}
