package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
)

func TestResolveGatewayInternalHost(t *testing.T) {
	const (
		gwName = "my-gateway"
		gwNS   = "istio-system"
	)

	newService := func(name string, port int32, ownerAPIVersion string) *corev1.Service {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: gwNS,
				Labels: map[string]string{
					"gateway.networking.k8s.io/gateway-name": gwName,
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: port}},
			},
		}
		if ownerAPIVersion != "" {
			svc.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: ownerAPIVersion,
				Kind:       "Gateway",
				Name:       gwName,
			}}
		}
		return svc
	}

	tests := []struct {
		name      string
		services  []runtime.Object
		wantHost  string
		wantError string
	}{
		{
			name: "single valid gateway-owned service",
			services: []runtime.Object{
				newService("gw-svc", 443, "gateway.networking.k8s.io/v1"),
			},
			wantHost: "gw-svc.istio-system.svc.cluster.local",
		},
		{
			name:      "no matching services",
			services:  []runtime.Object{},
			wantError: "no gateway-owned service with HTTPS port found",
		},
		{
			name: "service without ownerReference is skipped",
			services: []runtime.Object{
				newService("rogue-svc", 443, ""),
			},
			wantError: "no gateway-owned service with HTTPS port found",
		},
		{
			name: "service owned by Istio Gateway CRD is skipped",
			services: []runtime.Object{
				newService("istio-svc", 443, "networking.istio.io/v1alpha3"),
			},
			wantError: "no gateway-owned service with HTTPS port found",
		},
		{
			name: "service without port 443 is skipped",
			services: []runtime.Object{
				newService("http-svc", 80, "gateway.networking.k8s.io/v1"),
			},
			wantError: "no gateway-owned service with HTTPS port found",
		},
		{
			name: "multiple valid candidates returns error",
			services: []runtime.Object{
				newService("gw-svc-1", 443, "gateway.networking.k8s.io/v1"),
				newService("gw-svc-2", 443, "gateway.networking.k8s.io/v1beta1"),
			},
			wantError: "expected 1 gateway service",
		},
		{
			name: "mixed valid and invalid services selects only valid",
			services: []runtime.Object{
				newService("valid-svc", 443, "gateway.networking.k8s.io/v1"),
				newService("no-owner", 443, ""),
				newService("wrong-port", 8080, "gateway.networking.k8s.io/v1"),
				newService("istio-gw", 443, "networking.istio.io/v1"),
			},
			wantHost: "valid-svc.istio-system.svc.cluster.local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(tt.services...)

			host, err := config.ResolveGatewayInternalHost(context.Background(), clientset, gwName, gwNS)

			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				assert.Empty(t, host)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantHost, host)
			}
		})
	}
}
