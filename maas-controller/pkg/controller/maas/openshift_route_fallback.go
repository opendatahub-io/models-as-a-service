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
	"fmt"
	"net/url"
	"sort"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"

type openShiftRouteEndpoint struct {
	Protocol string
	Host     string
}

func (r *MaaSModelRefReconciler) withOpenShiftRouteFallback(
	ctx context.Context,
	log logr.Logger,
	endpoint string,
	gatewayName string,
	gatewayNamespace string,
) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse model endpoint %q: %w", endpoint, err)
	}
	if !isClusterLocalURL(endpoint) {
		return endpoint, nil
	}

	routeEndpoint, found, err := r.openShiftRouteEndpoint(ctx, log, gatewayName, gatewayNamespace)
	if err != nil {
		return "", err
	}
	if !found {
		return endpoint, nil
	}

	parsed.Scheme = routeEndpoint.Protocol
	parsed.Host = routeEndpoint.Host
	return parsed.String(), nil
}

func (r *MaaSModelRefReconciler) openShiftRouteEndpoint(
	ctx context.Context,
	log logr.Logger,
	gatewayName string,
	gatewayNamespace string,
) (openShiftRouteEndpoint, bool, error) {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(gatewayNamespace)); err != nil {
		return openShiftRouteEndpoint{}, false, fmt.Errorf(
			"list Services in namespace %s: %w", gatewayNamespace, err)
	}
	serviceNames := gatewayServiceNames(services, gatewayName)
	if len(serviceNames) == 0 {
		return openShiftRouteEndpoint{}, false, nil
	}

	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "route.openshift.io",
		Version: "v1",
		Kind:    "RouteList",
	})
	if err := r.List(ctx, routes, client.InNamespace(gatewayNamespace)); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			log.Info("OpenShift Route API unavailable; skipping Route fallback")
			return openShiftRouteEndpoint{}, false, nil
		}
		return openShiftRouteEndpoint{}, false, fmt.Errorf(
			"list Routes in namespace %s: %w", gatewayNamespace, err)
	}

	endpoints := make([]openShiftRouteEndpoint, 0)
	for _, route := range routes.Items {
		if endpoint, ok := admittedHostOnlyRouteEndpoint(route, serviceNames); ok {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return openShiftRouteEndpoint{}, false, nil
	}

	sort.SliceStable(endpoints, func(left, right int) bool {
		if endpoints[left].Protocol != endpoints[right].Protocol {
			return endpoints[left].Protocol == "https"
		}
		return endpoints[left].Host < endpoints[right].Host
	})
	return endpoints[0], true, nil
}

func gatewayServiceNames(services *corev1.ServiceList, gatewayName string) map[string]struct{} {
	serviceNames := make(map[string]struct{})
	for _, service := range services.Items {
		if service.Spec.Selector[gatewayNameLabel] == gatewayName {
			serviceNames[service.Name] = struct{}{}
		}
	}
	return serviceNames
}

func admittedHostOnlyRouteEndpoint(
	route unstructured.Unstructured,
	serviceNames map[string]struct{},
) (openShiftRouteEndpoint, bool) {
	targetKind, _, _ := unstructured.NestedString(route.Object, "spec", "to", "kind")
	if targetKind != "" && targetKind != "Service" {
		return openShiftRouteEndpoint{}, false
	}
	targetName, _, _ := unstructured.NestedString(route.Object, "spec", "to", "name")
	if _, targetsGateway := serviceNames[targetName]; !targetsGateway {
		return openShiftRouteEndpoint{}, false
	}
	targetWeight, weightFound, _ := unstructured.NestedInt64(route.Object, "spec", "to", "weight")
	if weightFound && targetWeight == 0 {
		return openShiftRouteEndpoint{}, false
	}
	routePath, pathFound, _ := unstructured.NestedString(route.Object, "spec", "path")
	if pathFound && routePath != "" {
		return openShiftRouteEndpoint{}, false
	}

	host := admittedRouteHost(route)
	if host == "" {
		return openShiftRouteEndpoint{}, false
	}
	protocol := "http"
	if _, hasTLS, _ := unstructured.NestedMap(route.Object, "spec", "tls"); hasTLS {
		protocol = "https"
	}
	return openShiftRouteEndpoint{Protocol: protocol, Host: host}, true
}

func admittedRouteHost(route unstructured.Unstructured) string {
	ingresses, found, err := unstructured.NestedSlice(route.Object, "status", "ingress")
	if err != nil || !found {
		return ""
	}
	for _, rawIngress := range ingresses {
		ingress, ok := rawIngress.(map[string]any)
		if !ok {
			continue
		}
		host, ok := ingress["host"].(string)
		if !ok || host == "" {
			continue
		}
		conditions, found, _ := unstructured.NestedSlice(ingress, "conditions")
		if !found {
			continue
		}
		for _, rawCondition := range conditions {
			condition, ok := rawCondition.(map[string]any)
			if !ok {
				continue
			}
			conditionType, _, _ := unstructured.NestedString(condition, "type")
			conditionStatus, _, _ := unstructured.NestedString(condition, "status")
			if conditionType == "Admitted" && conditionStatus == "True" {
				return host
			}
		}
	}
	return ""
}
